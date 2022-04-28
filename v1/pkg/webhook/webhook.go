package webhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	_ "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/resource"
	_ "k8s.io/apimachinery/pkg/runtime/schema"
	_ "k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"strconv"
	_ "strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	v1 "k8s.io/kubernetes/pkg/apis/core/v1"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

type Server struct {
	Server *http.Server
}

// Webhook Server parameters
type WhSvrParameters struct {
	Port           int    // webhook server port
	CertFile       string // path to the x509 certificate for https
	KeyFile        string // path to the x509 private key matching `CertFile`
	SidecarCfgFile string // path to sidecar injector configuration file
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	_ = v1.AddToScheme(runtimeScheme)
}

// validate deployments and services
func (whsvr *Server) validate(ar *v1beta1.AdmissionReview, log *bytes.Buffer) *v1beta1.AdmissionResponse {
	req := ar.Request
	action := string(req.Operation)
	glog.Info("\n======begin Admission for Namespace=[", req.Namespace, "], Kind=[", req.Kind.Kind, "], Name=[", req.Name, "], Action=[", action, "] ======")
	switch req.Kind.Kind {
	case "PersistentVolumeClaim":
		allowed := true
		var (
			storageVersion int
			quotaStorage   int64
			usageStorage   int64
			result         *metav1.Status
			pvc            corev1.PersistentVolumeClaim
			newPvc         corev1.PersistentVolumeClaim
			raw            []byte
		)
		if action == "CREATE" {
			raw = req.Object.Raw
		} else if action == "DELETE" {
			raw = req.OldObject.Raw
		} else if action == "UPDATE" {
			raw = req.OldObject.Raw
			if err := json.Unmarshal(req.Object.Raw, &newPvc); err != nil {
				log.WriteString(fmt.Sprintf("\nCould not unmarshal raw object: %v", err))
				glog.Errorf(log.String())
				return &v1beta1.AdmissionResponse{
					Result: &metav1.Status{
						Message: err.Error(),
					},
				}
			}
		}
		if err := json.Unmarshal(raw, &pvc); err != nil {
			log.WriteString(fmt.Sprintf("\nCould not unmarshal raw object: %v", err))
			glog.Errorf(log.String())
			return &v1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
		// 获取请求pvc的大小,
		storage := pvc.Spec.Resources.Requests["storage"]
		storageName := *pvc.Spec.StorageClassName
		pvcStorageSize := storage.Value()
		// update
		var (
			newStorage        resource.Quantity
			newPvcStorageSize int64
		)
		if action == "UPDATE" {
			newStorage = newPvc.Spec.Resources.Requests["storage"]
			newPvcStorageSize = newStorage.Value()
		}
		// 获取storageclass中记录的存储限额大小
		config, err := clientcmd.BuildConfigFromFlags("", "/root/.kube/config")
		if err != nil {
			panic(err)
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}
		//获取scList，所有存储类
		storageList, err := client.StorageV1beta1().StorageClasses().List(metav1.ListOptions{})
		if err != nil {
			panic(err)
		}
		for i := 0; i < len(storageList.Items); i++ {
			storageClass := storageList.Items[i]
			if storageName == storageClass.GetName() { // 判断是否是这个sc
				annotations := storageClass.GetAnnotations()
				for key, v := range annotations { // 循环查看annotation内容，获取存储相关的annotation
					if key == "quota" { // annotation为 quota: 总量 50Gi (53687091200Bi)
						decodeQuotaStorage, _ := base64.URLEncoding.DecodeString(v) // 解码
						quotaStorage, err = strconv.ParseInt((string(decodeQuotaStorage))[0:len(string(decodeQuotaStorage))-2], 10, 64)
						if err != nil {
							panic(err)
						}
					}
					if key == "usage" { // annotation为 usage：已用 45Gi (5368709120Bi)
						decodeUsageStorage, _ := base64.URLEncoding.DecodeString(v) // 解码
						usageStorage, err = strconv.ParseInt((string(decodeUsageStorage))[0:len(decodeUsageStorage)-2], 10, 64)
						if err != nil {
							panic(err)
						}
					}
					if key == "storageVersion" {
						storageVersion, err = strconv.Atoi(v)
						if err != nil {
							panic(err)
						}
					}
				}
				if action == "CREATE" {
					if pvcStorageSize+usageStorage > quotaStorage { // 超，拒绝
						allowed = false
						glog.Error("===Storage capacity out of bounds")
						glog.Info("===storage quota: ", FormatFileSize(strconv.FormatInt(quotaStorage, 10)+"Bi"))
						glog.Info("===storage usage: ", FormatFileSize(strconv.FormatInt(usageStorage, 10)+"Bi"))
						return &v1beta1.AdmissionResponse{
							Allowed: allowed,
							Result:  result,
						}
					} else { // 未超，准入,并修改usage值
						if annotations["storageVersion"] == strconv.Itoa(storageVersion) {
							usage := strconv.FormatInt(pvcStorageSize+usageStorage, 10) + "Bi"
							annotations["usage"] = base64.URLEncoding.EncodeToString([]byte(usage))
							annotations["storageVersion"] = strconv.Itoa(storageVersion + 1)
							storageClass.SetAnnotations(annotations)
							_, err = client.StorageV1beta1().StorageClasses().Update(&storageClass)
							if err != nil {
								glog.Warning("update storaClass annotation failed")
							}
							glog.Info("===storage quota: ", FormatFileSize(strconv.FormatInt(quotaStorage, 10)+"Bi"))
							glog.Info("===storage usage: ", FormatFileSize(usage))
							return &v1beta1.AdmissionResponse{
								Allowed: allowed,
								Result:  result,
							}
						} else { // 乐观锁，重试
							whsvr.validate(ar, log)
						}
					}
				} else if action == "DELETE" {
					if annotations["storageVersion"] == strconv.Itoa(storageVersion) {
						usage := strconv.FormatInt(usageStorage-pvcStorageSize, 10) + "Bi"
						annotations["usage"] = base64.URLEncoding.EncodeToString([]byte(usage))
						annotations["storageVersion"] = strconv.Itoa(storageVersion + 1)
						storageClass.SetAnnotations(annotations)
						_, err = client.StorageV1beta1().StorageClasses().Update(&storageClass)
						if err != nil {
							glog.Warning("update storaClass annotation failed")
						}
						glog.Info("===storage quota: ", FormatFileSize(strconv.FormatInt(quotaStorage, 10)+"Bi"))
						glog.Info("===storage usage: ", FormatFileSize(usage))
						return &v1beta1.AdmissionResponse{
							Allowed: allowed,
							Result:  result,
						}
					} else { // 乐观锁，重试
						whsvr.validate(ar, log)
					}
				} else if action == "UPDATE" { // TODO update为何会在每次新建和删除时重复执行多次？？？
					var expansion = false
					glog.Info("storageClass.AllowVolumeExpansion: ", storageClass.AllowVolumeExpansion)
					if storageClass.AllowVolumeExpansion != nil {
						expansion = *storageClass.AllowVolumeExpansion
					}
					if storageClass.AllowedTopologies == nil || !expansion { // 不可扩展，直接拒绝
						allowed = false
						return &v1beta1.AdmissionResponse{
							Allowed: allowed,
							Result:  result,
						}
					} else {
						if newPvcStorageSize > pvcStorageSize {
							addSize := newPvcStorageSize - pvcStorageSize
							if addSize+usageStorage > quotaStorage { // 超，拒绝
								allowed = false
								glog.Error("===Storage capacity out of bounds")
								glog.Info("===storage quota: ", FormatFileSize(annotations["quota"]))
								glog.Info("===storage usage: ", FormatFileSize(annotations["usage"]))
								return &v1beta1.AdmissionResponse{
									Allowed: allowed,
									Result:  result,
								}
							} else { // 未超，准入,并修改usage值
								annotations["usage"] = strconv.FormatInt(addSize+usageStorage, 10) + "Bi"
								storageClass.SetAnnotations(annotations)
								_, err = client.StorageV1beta1().StorageClasses().Update(&storageClass)
								if err != nil {
									glog.Warning("update storaClass annotation failed")
								}
								glog.Info("===storage quota: ", FormatFileSize(annotations["quota"]))
								glog.Info("===storage usage: ", FormatFileSize(annotations["usage"]))
								return &v1beta1.AdmissionResponse{
									Allowed: allowed,
									Result:  result,
								}
							}
						}
					}
				}
			}
		}

	//其他不支持的类型
	default:
		msg := fmt.Sprintf("\nNot support for this Kind of resource  %v", req.Kind.Kind)
		log.WriteString(msg)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: msg,
			},
		}
	}
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{},
	}
}

// Serve method for webhook server
func (whsvr *Server) Serve(w http.ResponseWriter, r *http.Request) {
	var log bytes.Buffer
	//读取从ApiServer过来的数据放到body
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		log.WriteString("empty body")
		glog.Info(log.String())
		//返回状态码400
		//如果在Apiserver调用此Webhook返回是400，说明APIServer自己传过来的数据是空
		http.Error(w, log.String(), http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.WriteString(fmt.Sprintf("Content-Type=%s, expect `application/json`", contentType))
		glog.Errorf(log.String())
		//如果在Apiserver调用此Webhook返回是415，说明APIServer自己传过来的数据不是json格式，处理不了
		http.Error(w, log.String(), http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		//组装错误信息
		log.WriteString(fmt.Sprintf("\nCan't decode body,error info is :  %s", err.Error()))
		glog.Errorln(log.String())
		//返回错误信息，形式表现为资源创建会失败，
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: log.String(),
			},
		}
	} else {
		fmt.Println(r.URL.Path)
		if r.URL.Path == "/mutate" {
			//admissionResponse = whsvr.mutate(&ar, &log)
		} else if r.URL.Path == "/validate" {
			admissionResponse = whsvr.validate(&ar, &log)
		}
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		log.WriteString(fmt.Sprintf("\nCan't encode response: %v", err))
		http.Error(w, log.String(), http.StatusInternalServerError)
	}
	glog.Infof("Ready to write reponse ...")
	if _, err := w.Write(resp); err != nil {
		log.WriteString(fmt.Sprintf("\nCan't write response: %v", err))
		http.Error(w, log.String(), http.StatusInternalServerError)
	}

	log.WriteString("\n======ended Admission already writed to reponse======")
	//东八区时间
	datetime := time.Now().In(time.FixedZone("GMT", 8*3600)).Format("2006-01-02 15:04:05")
	//最后打印日志
	glog.Infof(datetime + " " + log.String())
}

/**
字节转换函数
*/
func FormatFileSize(storeSizeStr string) (size string) {
	sizeNum, _ := strconv.Atoi(storeSizeStr[0 : len(storeSizeStr)-2])
	sizeUnit := storeSizeStr[len(storeSizeStr)-2:]

	switch sizeUnit {
	case "Bi":
		if sizeNum < 1024 {
			return fmt.Sprintf("%.2fBi", float64(sizeNum)/float64(1))
		} else if sizeNum < (1024 * 1024) {
			return fmt.Sprintf("%.2fKi", float64(sizeNum)/float64(1024))
		} else if sizeNum < (1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fMi", float64(sizeNum)/float64(1024*1024))
		} else if sizeNum < (1024 * 1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fGi", float64(sizeNum)/float64(1024*1024*1024))
		} else if sizeNum < (1024 * 1024 * 1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fTi", float64(sizeNum)/float64(1024*1024*1024*1024))
		} else {
			return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1024*1024*1024*1024*1024))
		}
	case "Ki":
		if sizeNum < 1024 {
			return fmt.Sprintf("%.2fKi", float64(sizeNum)/float64(1))
		} else if sizeNum < (1024 * 1024) {
			return fmt.Sprintf("%.2fMi", float64(sizeNum)/float64(1024))
		} else if sizeNum < (1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fGi", float64(sizeNum)/float64(1024*1024))
		} else if sizeNum < (1024 * 1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fTi", float64(sizeNum)/float64(1024*1024*1024))
		} else {
			return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1024*1024*1024*1024))
		}
	case "Mi":
		if sizeNum < 1024 {
			return fmt.Sprintf("%.2fMi", float64(sizeNum)/float64(1))
		} else if sizeNum < (1024 * 1024) {
			return fmt.Sprintf("%.2fGi", float64(sizeNum)/float64(1024))
		} else if sizeNum < (1024 * 1024 * 1024) {
			return fmt.Sprintf("%.2fTi", float64(sizeNum)/float64(1024*1024))
		} else {
			return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1024*1024*1024))
		}
	case "Gi":
		if sizeNum < 1024 {
			return fmt.Sprintf("%.2fGi", float64(sizeNum)/float64(1))
		} else if sizeNum < (1024 * 1024) {
			return fmt.Sprintf("%.2fTi", float64(sizeNum)/float64(1024))
		} else {
			return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1024*1024))
		}
	case "Ti":
		if sizeNum < 1024 {
			return fmt.Sprintf("%.2fTi", float64(sizeNum)/float64(1))
		} else {
			return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1024))
		}
	case "Ei":
		return fmt.Sprintf("%.2fEi", float64(sizeNum)/float64(1))
	default:
		return ""
	}
}
