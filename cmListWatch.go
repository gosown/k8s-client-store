package k8s_client_store

import (
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
)

// newConfigMapListWatcher 用于创建 tmp、namespace 下 configmap 资源的 ListWatcher 实例
func NewConfigMapListWatcher() *cache.ListWatch {
	clientset := NewClientset()
	client := clientset.CoreV1().RESTClient()
	resource := "configmaps"
	namespace := "tmp"
	selector := fields.Everything()
	lw := cache.NewListWatchFromClient(client, resource, namespace, selector)
	return lw
}
