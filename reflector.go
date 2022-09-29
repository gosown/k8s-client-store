package k8s_client_store

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

func NewConfigMapsReflector(queue cache.Queue) *cache.Reflector {
	lw := NewConfigMapsListWatcher()
	return cache.NewReflector(lw, &corev1.ConfigMap{}, queue, 0)
}
