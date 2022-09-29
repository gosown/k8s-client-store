package main

import (
	"fmt"
	k8s_client_store "github.com/gosown/k8s-client-store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"os/signal"
)

func main() {
	fmt.Println("------------ 1-list-watcher ------------")
	lw := k8s_client_store.NewConfigMapsListWatcher()

	list, err := lw.List(metav1.ListOptions{})
	if err != nil {
		panic(err)
	}

	items, err := meta.ExtractList(list)
	if err != nil {
		panic(err)
	}

	for _, item := range items {
		configMap, ok := item.(*corev1.ConfigMap)
		if !ok {
			return
		}
		fmt.Println(configMap.Name)
	}

	listMetaInterface, err := meta.ListAccessor(list)
	if err != nil {
		panic(err)
	}

	resourceVersion := listMetaInterface.GetResourceVersion()
	w, err := lw.Watch(metav1.ListOptions{ResourceVersion: resourceVersion})
	if err != nil {
		panic(err)
	}

	stopCh := make(chan os.Signal)
	signal.Notify(stopCh, os.Interrupt)

	fmt.Println("Start watching...")

loop:
	for {
		select {
		case <-stopCh:
			fmt.Println("Interrupted")
			break loop
		case event, ok := <-w.ResultChan():
			if !ok {
				fmt.Println("Broken channel")
				break loop
			}
			configMap, ok := event.Object.(*corev1.ConfigMap)
			if !ok {
				return
			}
			fmt.Printf("%s: %s\n", event.Type, configMap.Name)
		}
	}
}
