package k8s_client_store

import "k8s.io/client-go/tools/cache"

func NewStore() cache.Store {
	return cache.NewStore(cache.MetaNamespaceKeyFunc)
}
