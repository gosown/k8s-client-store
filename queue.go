package k8s_client_store

import "k8s.io/client-go/tools/cache"

func NewQueue(store cache.Store) cache.Queue {
	return cache.NewDeltaFIFOWithOptions(cache.DeltaFIFOOptions{
		KnownObjects:          store,
		EmitDeltaTypeReplaced: true,
	})
}
