package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cnych/admission-webhook/pkg/webhook"
	"github.com/golang/glog"
	cron "github.com/robfig/cron/v3"
)

func main() {
	var parameters webhook.WhSvrParameters

	// get command line parameters
	flag.IntVar(&parameters.Port, "port", 443, "Webhook server port.")
	flag.StringVar(&parameters.CertFile, "tlsCertFile", "/etc/webhook/certs/cert.pem", "File containing the x509 Certificate for HTTPS.")
	flag.StringVar(&parameters.KeyFile, "tlsKeyFile", "/etc/webhook/certs/key.pem", "File containing the x509 private key to --tlsCertFile.")
	flag.Parse()

	pair, err := tls.LoadX509KeyPair(parameters.CertFile, parameters.KeyFile)
	if err != nil {
		glog.Errorf("Failed to load key pair: %v", err)
	}
	//启动httpserver
	whsvr := &webhook.Server{
		Server: &http.Server{
			Addr:      fmt.Sprintf(":%v", parameters.Port),
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
		},
	}
	// define http server and server handler // 注册handler
	mux := http.NewServeMux()
	//mux.HandleFunc("/mutate", whsvr.Serve)
	mux.HandleFunc("/validate", whsvr.Serve)
	whsvr.Server.Handler = mux
	// start webhook server in new routine // 启动协程来处理
	go func() {
		if err := whsvr.Server.ListenAndServeTLS("", ""); err != nil {
			glog.Errorf("Failed to listen and serve webhook server: %v", err)
		}
	}()

	glog.Info("Server started...")

	go synchronousStorage()

	// listening OS shutdown singal
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	glog.Infof("Got OS shutdown signal, shutting down webhook server gracefully...")
	_ = whsvr.Server.Shutdown(context.Background())

}
func synchronousStorage() {
	crontab := cron.New()
	task := func() {
		config, err := clientcmd.BuildConfigFromFlags("", "/root/.kube/config")
		if err != nil {
			panic(err)
		}
		client, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}
		// 获取所有pvc
		//persistentVolumes, err := client.CoreV1().PersistentVolumeClaims("").List(metav1.ListOptions{})
		persistentVolumes, err := client.CoreV1().PersistentVolumes().List(metav1.ListOptions{})

		if err != nil {
			panic(err)
		}
		//获取所有存储类
		storageList, err := client.StorageV1beta1().StorageClasses().List(metav1.ListOptions{})
		if err != nil {
			panic(err)
		}
		for i := 0; i < len(storageList.Items); i++ {
			storageClass := storageList.Items[i]
			annotations := storageClass.GetAnnotations()
			storageClassName := storageClass.GetName()
			var (
				usageStorage   int64
				pvcCount       int64 = 0
				storageVersion int
				quotaStorage   int64
			)
			for key, v := range annotations {
				if key == "quota" { // annotation为 quota: 总量 50Gi (53687091200Bi)
					decodeQuotaStorage, _ := base64.URLEncoding.DecodeString(v) // 解码
					quotaStorage, err = strconv.ParseInt((string(decodeQuotaStorage))[0:len(string(decodeQuotaStorage))-2], 10, 64)
					if err != nil {
						panic(err)
					}
				}
				if key == "usage" {
					decodeUsageStorage, _ := base64.URLEncoding.DecodeString(v) // 解码
					usageStorage, err = strconv.ParseInt((string(decodeUsageStorage))[0:len(string(decodeUsageStorage))-2], 10, 64)
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
			for j := 0; j < len(persistentVolumes.Items); j++ {
				pv := persistentVolumes.Items[j]
				pvcStorageClassName := pv.Spec.StorageClassName
				pvcStorage := pv.Spec.Capacity["storage"]
				pvcStorageNum := pvcStorage.Value()
				if storageClassName == pvcStorageClassName {
					pvcCount = pvcCount + pvcStorageNum
				}
			}
			if pvcCount != usageStorage { // 存储类中的usage和pvc总量不匹配，更新usage值
				if annotations["storageVersion"] == strconv.Itoa(storageVersion) {
					usage := strconv.FormatInt(pvcCount, 10) + "Bi"
					annotations["usage"] = base64.URLEncoding.EncodeToString([]byte(usage))
					storageClass.SetAnnotations(annotations)
					_, err = client.StorageV1beta1().StorageClasses().Update(&storageClass)
					if err != nil {
						glog.Warning("update storaClass annotation failed")
					}
				}
			}
			datetime := time.Now().In(time.FixedZone("GMT", 8*3600)).Format("2006-01-02 15:04:05")
			glog.Infof(datetime + " 同步存储类 " + storageClassName + " 存储卷容量")
			glog.Info("===storage quota: ", webhook.FormatFileSize(strconv.FormatInt(quotaStorage, 10)+"Bi"))
			glog.Info("===storage usage: ", webhook.FormatFileSize(strconv.FormatInt(pvcCount, 10)+"Bi"))
		}
	}
	_, _ = crontab.AddFunc("0/10 * * * *", task)
	// 启动定时器
	crontab.Start()
}
