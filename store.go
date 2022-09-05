package k8s_client_store

import (
	"context"
	"fmt"
	"github.com/eapache/channels"
	"github.com/gosown/k8s-client-store/k8s"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"reflect"
	"sync"
	"time"
)

type Storer interface {
	// GetConfigMap returns the ConfigMap matching key.
	GetConfigMap(key string) (*corev1.ConfigMap, error)

	// GetSecret returns the Secret matching key.
	GetSecret(key string) (*corev1.Secret, error)

	// GetService returns the Service matching key.
	GetService(key string) (*corev1.Service, error)

	// GetServiceEndpoints returns the Endpoints of a Service matching key.
	GetServiceEndpoints(key string) (*corev1.Endpoints, error)

	// Run initiates the synchronization of the controllers
	Run(stopCh chan struct{})
}

// k8sStore internal Storer implementation using informers and thread safe stores
type k8sStore struct {

	// informer contains the cache Informers
	informers *Informer

	// listers contains the cache.Store interfaces used in the ingress controller
	listers *Lister

	// secretIngressMap contains information about which ingress references a
	// secret in the annotations.
	// secretIngressMap 包含有关哪些入口引用 annotations 中的秘密的信息。
	secretIngressMap ObjectRefMap

	// updateCh
	updateCh *channels.RingChannel

	// syncSecretMu protects against simultaneous invocations of syncSecret
	syncSecretMu *sync.Mutex

	// backendConfigMu protects against simultaneous read/write of backendConfig
	backendConfigMu *sync.RWMutex

	defaultSSLCertificate string
}

// Informer defines the required SharedIndexInformers that interact with the API server.
type Informer struct {
	Ingress      cache.SharedIndexInformer
	IngressClass cache.SharedIndexInformer
	Endpoint     cache.SharedIndexInformer
	Service      cache.SharedIndexInformer
	Secret       cache.SharedIndexInformer
	ConfigMap    cache.SharedIndexInformer
	Namespace    cache.SharedIndexInformer
}

// Lister contains object listers (stores).
type Lister struct {
	Service   ServiceLister
	Endpoint  EndpointLister
	Secret    SecretLister
	ConfigMap ConfigMapLister
	Namespace NamespaceLister
}

// NotExistsError is returned when an object does not exist in a local store.
type NotExistsError string

// EventType type of event associated with an informer
type EventType string

// Event holds the context of an event.
type Event struct {
	Type EventType
	Obj  interface{}
}

const (
	// CreateEvent event associated with new objects in an informer
	// 与 informer 中的新对象关联的 CreateEvent 事件
	CreateEvent EventType = "CREATE"
	// UpdateEvent event associated with an object update in an informer
	// 与 informer 中的对象更新关联的 UpdateEvent 事件
	UpdateEvent EventType = "UPDATE"
	// DeleteEvent event associated when an object is removed from an informer
	// 当对象从 informer 中移除时关联的 DeleteEvent 事件
	DeleteEvent EventType = "DELETE"
	// ConfigurationEvent event associated when a controller configuration object is created or updated
	// 创建或更新控制器配置对象时关联的 ConfigurationEvent 事件
	ConfigurationEvent EventType = "CONFIGURATION"
)

// Error implements the error interface.
func (e NotExistsError) Error() string {
	return fmt.Sprintf("no object matching key %q in local store", string(e))
}

// hasCatchAllIngressRule returns whether or not an ingress produces a
// catch-all server, and so should be ignored when --disable-catch-all is set
func hasCatchAllIngressRule(spec networkingv1.IngressSpec) bool {
	return spec.DefaultBackend != nil
}

func New(
	namespace string,
	namespaceSelector labels.Selector,
	configmap, tcp, udp, defaultSSLCertificate string,
	resyncPeriod time.Duration,
	client clientset.Interface,
	updateCh *channels.RingChannel,
	disableCatchAll bool,
	deepInspector bool) Storer {

	store := &k8sStore{
		informers:             &Informer{},
		listers:               &Lister{},
		updateCh:              updateCh,
		syncSecretMu:          &sync.Mutex{},
		backendConfigMu:       &sync.RWMutex{},
		defaultSSLCertificate: defaultSSLCertificate,
	}

	// 教程：
	// https://www.infoq.cn/article/tocm0g8zr19kj3olw8c4
	// https://www.cnblogs.com/luozhiyun/p/13799901.html

	// StartEventWatcher()：EventBroadcaster 中的核心方法，接收各模块产生的 events，参数为一个处理 events 的函数，用户可以使用 StartEventWatcher() 接收 events 然后使用自定义的 handle 进行处理

	// 创建事件广播
	eventBroadcaster := record.NewBroadcaster()
	// 也是调用 StartEventWatcher() 接收 events，然后保存 events 到日志
	eventBroadcaster.StartLogging(klog.Infof)
	// 将event发送给apiserver
	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{
		Interface: client.CoreV1().Events(namespace),
	})
	// 初始化 EventRecorder 对象，EventRecorder 对象会生成 events 并通过 Action() 方法发送 events 到 Broadcaster 的 channel 队列中；
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: "nginx-ingress-controller",
	})

	// ???
	// store.listers.IngressWithAnnotation.Store = cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)

	labelsTweakListOptionsFunc := func(options *metav1.ListOptions) {
		if len(options.LabelSelector) > 0 {
			options.LabelSelector += ",OWNER!=TILLER"
		} else {
			options.LabelSelector = "OWNER!=TILLER"
		}
	}

	// As of HELM >= v3 helm releases are stored using Secrets instead of ConfigMaps.
	// In order to avoid listing those secrets we discard type "helm.sh/release.v1"
	secretsTweakListOptionsFunc := func(options *metav1.ListOptions) {
		helmAntiSelector := fields.OneTermNotEqualSelector("type", "helm.sh/release.v1")
		baseSelector, err := fields.ParseSelector(options.FieldSelector)

		if err != nil {
			options.FieldSelector = helmAntiSelector.String()
		} else {
			options.FieldSelector = fields.AndSelectors(baseSelector, helmAntiSelector).String()
		}
	}

	// create informers factory, enable and assign required informers
	infFactory := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
		informers.WithNamespace(namespace),
	)

	// create informers factory for configmaps
	infFactoryConfigmaps := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(labelsTweakListOptionsFunc),
	)

	// create informers factory for secrets
	infFactorySecrets := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(secretsTweakListOptionsFunc),
	)

	store.informers.Ingress = infFactory.Networking().V1().Ingresses().Informer()
	//store.listers.Ingress.Store = store.informers.Ingress.GetStore()

	//if !icConfig.IgnoreIngressClass {
	//	store.informers.IngressClass = infFactory.Networking().V1().IngressClasses().Informer()
	//store.listers.IngressClass.Store = cache.NewStore(cache.MetaNamespaceKeyFunc)
	//}

	store.informers.Endpoint = infFactory.Core().V1().Endpoints().Informer()
	store.listers.Endpoint.Store = store.informers.Endpoint.GetStore()

	store.informers.Secret = infFactorySecrets.Core().V1().Secrets().Informer()
	store.listers.Secret.Store = store.informers.Secret.GetStore()

	store.informers.ConfigMap = infFactoryConfigmaps.Core().V1().ConfigMaps().Informer()
	store.listers.ConfigMap.Store = store.informers.ConfigMap.GetStore()

	store.informers.Service = infFactory.Core().V1().Services().Informer()
	store.listers.Service.Store = store.informers.Service.GetStore()

	// avoid caching namespaces at cluster scope when watching single namespace
	if namespaceSelector != nil && !namespaceSelector.Empty() {
		// cache informers factory for namespaces
		infFactoryNamespaces := informers.NewSharedInformerFactoryWithOptions(client, resyncPeriod,
			informers.WithTweakListOptions(labelsTweakListOptionsFunc),
		)

		store.informers.Namespace = infFactoryNamespaces.Core().V1().Namespaces().Informer()
		store.listers.Namespace.Store = store.informers.Namespace.GetStore()
	}

	watchedNamespace := func(namespace string) bool {
		if namespaceSelector == nil || namespaceSelector.Empty() {
			return true
		}

		item, ok, err := store.listers.Namespace.GetByKey(namespace)
		if !ok {
			klog.Errorf("Namespace %s not existed: %v.", namespace, err)
			return false
		}
		ns, ok := item.(*corev1.Namespace)
		if !ok {
			return false
		}

		return namespaceSelector.Matches(labels.Set(ns.Labels))
	}

	ingDeleteHandler := func(obj interface{}) {
		ing, ok := toIngress(obj)
		if !ok {
			// If we reached here it means the ingress was deleted but its final state is unrecorded.
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if !ok {
				klog.ErrorS(nil, "Error obtaining object from tombstone", "key", obj)
				return
			}
			ing, ok = tombstone.Obj.(*networkingv1.Ingress)
			if !ok {
				klog.Errorf("Tombstone contained object that is not an Ingress: %#v", obj)
				return
			}
		}

		if !watchedNamespace(ing.Namespace) {
			return
		}

		//_, err := store.GetIngressClass(ing, icConfig)
		//if err != nil {
		//	klog.InfoS("Ignoring ingress because of error while validating ingress class", "ingress", klog.KObj(ing), "error", err)
		//	return
		//}

		if hasCatchAllIngressRule(ing.Spec) && disableCatchAll {
			klog.InfoS("Ignoring delete for catch-all because of --disable-catch-all", "ingress", klog.KObj(ing))
			return
		}
		// 注解，https://kubernetes.io/zh-cn/docs/concepts/overview/working-with-objects/annotations/
		//store.listers.IngressWithAnnotation.Delete(ing)

		key := k8s.MetaNamespaceKey(ing)
		store.secretIngressMap.Delete(key)

		updateCh.In() <- Event{
			Type: DeleteEvent,
			Obj:  obj,
		}
	}

	ingEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing, _ := toIngress(obj)

			if !watchedNamespace(ing.Namespace) {
				return
			}

			//ic, err := store.GetIngressClass(ing, icConfig)
			//if err != nil {
			//	klog.InfoS("Ignoring ingress because of error while validating ingress class", "ingress", klog.KObj(ing), "error", err)
			//	return
			//}

			//klog.InfoS("Found valid IngressClass", "ingress", klog.KObj(ing), "ingressclass", ic)

			if hasCatchAllIngressRule(ing.Spec) && disableCatchAll {
				klog.InfoS("Ignoring add for catch-all ingress because of --disable-catch-all", "ingress", klog.KObj(ing))
				return
			}

			recorder.Eventf(ing, corev1.EventTypeNormal, "Sync", "Scheduled for sync")

			store.syncIngress(ing)
			store.updateSecretIngressMap(ing)
			store.syncSecrets(ing)

			updateCh.In() <- Event{
				Type: CreateEvent,
				Obj:  obj,
			}
		},
		DeleteFunc: ingDeleteHandler,
		UpdateFunc: func(old, cur interface{}) {
			oldIng, _ := toIngress(old)
			curIng, _ := toIngress(cur)

			if !watchedNamespace(oldIng.Namespace) {
				return
			}

			var errOld, errCur error
			var classCur string
			//if !icConfig.IgnoreIngressClass {
			//	_, errOld = store.GetIngressClass(oldIng, icConfig)
			//	classCur, errCur = store.GetIngressClass(curIng, icConfig)
			//}
			if errOld != nil && errCur == nil {
				if hasCatchAllIngressRule(curIng.Spec) && disableCatchAll {
					klog.InfoS("ignoring update for catch-all ingress because of --disable-catch-all", "ingress", klog.KObj(curIng))
					return
				}

				klog.InfoS("creating ingress", "ingress", klog.KObj(curIng), "ingressclass", classCur)
				recorder.Eventf(curIng, corev1.EventTypeNormal, "Sync", "Scheduled for sync")
			} else if errOld == nil && errCur != nil {
				klog.InfoS("removing ingress because of unknown ingressclass", "ingress", klog.KObj(curIng))
				ingDeleteHandler(old)
				return
			} else if errCur == nil && !reflect.DeepEqual(old, cur) {
				if hasCatchAllIngressRule(curIng.Spec) && disableCatchAll {
					klog.InfoS("ignoring update for catch-all ingress and delete old one because of --disable-catch-all", "ingress", klog.KObj(curIng))
					ingDeleteHandler(old)
					return
				}

				recorder.Eventf(curIng, corev1.EventTypeNormal, "Sync", "Scheduled for sync")
			} else {
				klog.V(3).InfoS("No changes on ingress. Skipping update", "ingress", klog.KObj(curIng))
				return
			}

			if deepInspector {

			}

			store.syncIngress(curIng)
			store.updateSecretIngressMap(curIng)
			store.syncSecrets(curIng)

			updateCh.In() <- Event{
				Type: UpdateEvent,
				Obj:  cur,
			}
		},
	}

	secrEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sec := obj.(*corev1.Secret)
			key := k8s.MetaNamespaceKey(sec)

			// find references in ingresses and update local ssl certs
			if ings := store.secretIngressMap.Reference(key); len(ings) > 0 {
				klog.InfoS("Secret was added and it is used in ingress annotations. Parsing", "secret", key)

				updateCh.In() <- Event{
					Type: CreateEvent,
					Obj:  obj,
				}
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				sec := cur.(*corev1.Secret)
				key := k8s.MetaNamespaceKey(sec)

				if !watchedNamespace(sec.Namespace) {
					return
				}

				// find references in ingresses and update local ssl certs
				if ings := store.secretIngressMap.Reference(key); len(ings) > 0 {
					klog.InfoS("secret was updated and it is used in ingress annotations. Parsing", "secret", key)

					updateCh.In() <- Event{
						Type: UpdateEvent,
						Obj:  cur,
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			sec, ok := obj.(*corev1.Secret)
			if !ok {
				// If we reached here it means the secret was deleted but its final state is unrecorded.
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}

				sec, ok = tombstone.Obj.(*corev1.Secret)
				if !ok {
					return
				}
			}

			if !watchedNamespace(sec.Namespace) {
				return
			}

			key := k8s.MetaNamespaceKey(sec)

			// find references in ingresses
			if ings := store.secretIngressMap.Reference(key); len(ings) > 0 {
				klog.InfoS("secret was deleted and it is used in ingress annotations. Parsing", "secret", key)

				updateCh.In() <- Event{
					Type: DeleteEvent,
					Obj:  obj,
				}
			}
		},
	}

	epEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			updateCh.In() <- Event{
				Type: CreateEvent,
				Obj:  obj,
			}
		},
		DeleteFunc: func(obj interface{}) {
			updateCh.In() <- Event{
				Type: DeleteEvent,
				Obj:  obj,
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			oep := old.(*corev1.Endpoints)
			cep := cur.(*corev1.Endpoints)
			if !reflect.DeepEqual(cep.Subsets, oep.Subsets) {
				updateCh.In() <- Event{
					Type: UpdateEvent,
					Obj:  cur,
				}
			}
		},
	}

	// TODO: add e2e test to verify that changes to one or more configmap trigger an update
	changeTriggerUpdate := func(name string) bool {
		return name == configmap || name == tcp || name == udp
	}

	handleCfgMapEvent := func(key string, cfgMap *corev1.ConfigMap, eventName string) {
		// updates to configuration configmaps can trigger an update
		triggerUpdate := false
		if changeTriggerUpdate(key) {
			triggerUpdate = true
			recorder.Eventf(cfgMap, corev1.EventTypeNormal, eventName, fmt.Sprintf("ConfigMap %v", key))
			if key == configmap {
				store.setConfig(cfgMap)
			}
		}

		//ings := store.listers.IngressWithAnnotation.List()
		//for _, ingKey := range ings {
		//	key := k8s.MetaNamespaceKey(ingKey)
		//	ing, err := store.getIngress(key)
		//	if err != nil {
		//		klog.Errorf("could not find Ingress %v in local store: %v", key, err)
		//		continue
		//	}
		//
		//	if triggerUpdate {
		//		store.syncIngress(ing)
		//	}
		//}

		if triggerUpdate {
			updateCh.In() <- Event{
				Type: ConfigurationEvent,
				Obj:  cfgMap,
			}
		}
	}

	cmEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cfgMap := obj.(*corev1.ConfigMap)
			key := k8s.MetaNamespaceKey(cfgMap)
			handleCfgMapEvent(key, cfgMap, "CREATE")
		},
		UpdateFunc: func(old, cur interface{}) {
			if reflect.DeepEqual(old, cur) {
				return
			}

			cfgMap := cur.(*corev1.ConfigMap)
			key := k8s.MetaNamespaceKey(cfgMap)
			handleCfgMapEvent(key, cfgMap, "UPDATE")
		},
	}

	serviceHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			if svc.Spec.Type == corev1.ServiceTypeExternalName {
				updateCh.In() <- Event{
					Type: CreateEvent,
					Obj:  obj,
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*corev1.Service)
			if svc.Spec.Type == corev1.ServiceTypeExternalName {
				updateCh.In() <- Event{
					Type: DeleteEvent,
					Obj:  obj,
				}
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			oldSvc := old.(*corev1.Service)
			curSvc := cur.(*corev1.Service)

			if reflect.DeepEqual(oldSvc, curSvc) {
				return
			}

			updateCh.In() <- Event{
				Type: UpdateEvent,
				Obj:  cur,
			}
		},
	}

	store.informers.Ingress.AddEventHandler(ingEventHandler)
	//if !icConfig.IgnoreIngressClass {
	//	store.informers.IngressClass.AddEventHandler(ingressClassEventHandler)
	//}
	store.informers.Endpoint.AddEventHandler(epEventHandler)
	store.informers.Secret.AddEventHandler(secrEventHandler)
	store.informers.ConfigMap.AddEventHandler(cmEventHandler)
	store.informers.Service.AddEventHandler(serviceHandler)

	// do not wait for informers to read the configmap configuration
	ns, name, _ := k8s.ParseNameNS(configmap)
	// 这里拿到 ConfigMap 配置信息
	cm, err := client.CoreV1().ConfigMaps(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Unexpected error reading configuration configmap: %v", err)
	}

	store.setConfig(cm)
	return store
}

// syncIngress parses ingress annotations converting the value of the
// annotation to a go struct
func (s *k8sStore) syncIngress(ing *networkingv1.Ingress) {
	key := k8s.MetaNamespaceKey(ing)
	klog.V(3).Infof("updating annotations information for ingress %v", key)

	copyIng := &networkingv1.Ingress{}
	ing.ObjectMeta.DeepCopyInto(&copyIng.ObjectMeta)

	ing.Spec.DeepCopyInto(&copyIng.Spec)
	ing.Status.DeepCopyInto(&copyIng.Status)

	for ri, rule := range copyIng.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		for pi, path := range rule.HTTP.Paths {
			if path.Path == "" {
				copyIng.Spec.Rules[ri].HTTP.Paths[pi].Path = "/"
			}
		}
	}

	k8s.SetDefaultNGINXPathType(copyIng)
}

// updateSecretIngressMap takes an Ingress and updates all Secret objects it
// references in secretIngressMap.
func (s *k8sStore) updateSecretIngressMap(ing *networkingv1.Ingress) {
	key := k8s.MetaNamespaceKey(ing)
	klog.V(3).Infof("updating references to secrets for ingress %v", key)

	// delete all existing references first
	s.secretIngressMap.Delete(key)

	var refSecrets []string

	for _, tls := range ing.Spec.TLS {
		secrName := tls.SecretName
		if secrName != "" {
			secrKey := fmt.Sprintf("%v/%v", ing.Namespace, secrName)
			refSecrets = append(refSecrets, secrKey)
		}
	}

	// We can not rely on cached ingress annotations because these are
	// discarded when the referenced secret does not exist in the local
	// store. As a result, adding a secret *after* the ingress(es) which
	// references it would not trigger a resync of that secret.
	secretAnnotations := []string{
		"auth-secret",
		"auth-tls-secret",
		"proxy-ssl-secret",
		"secure-verify-ca-secret",
	}
	for _, ann := range secretAnnotations {
		secrKey, _ := objectRefAnnotationNsKey(ann, ing)

		if secrKey != "" {
			refSecrets = append(refSecrets, secrKey)
		}
	}

	// populate map with all secret references
	s.secretIngressMap.Insert(key, refSecrets...)
}

// objectRefAnnotationNsKey returns an object reference formatted as a
// 'namespace/name' key from the given annotation name.
func objectRefAnnotationNsKey(ann string, ing *networkingv1.Ingress) (string, error) {

	return "", nil
}

// syncSecrets synchronizes data from all Secrets referenced by the given
// Ingress with the local store and file system.
func (s *k8sStore) syncSecrets(ing *networkingv1.Ingress) {

}

// GetSecret returns the Secret matching key.
func (s *k8sStore) GetSecret(key string) (*corev1.Secret, error) {
	return s.listers.Secret.ByKey(key)
}

// GetService returns the Service matching key.
func (s *k8sStore) GetService(key string) (*corev1.Service, error) {
	return s.listers.Service.ByKey(key)
}

// GetConfigMap returns the ConfigMap matching key.
func (s *k8sStore) GetConfigMap(key string) (*corev1.ConfigMap, error) {
	return s.listers.ConfigMap.ByKey(key)
}

// GetServiceEndpoints returns the Endpoints of a Service matching key.
func (s *k8sStore) GetServiceEndpoints(key string) (*corev1.Endpoints, error) {
	return s.listers.Endpoint.ByKey(key)
}

func (s *k8sStore) setConfig(cmap *corev1.ConfigMap) {

}

// Run initiates the synchronization of the informers and the initial
// synchronization of the secrets.
func (s *k8sStore) Run(stopCh chan struct{}) {
	// start informers
	s.informers.Run(stopCh)
}

var runtimeScheme = k8sruntime.NewScheme()

func init() {
	utilruntime.Must(networkingv1.AddToScheme(runtimeScheme))
}

func toIngress(obj interface{}) (*networkingv1.Ingress, bool) {
	if ing, ok := obj.(*networkingv1.Ingress); ok {
		k8s.SetDefaultNGINXPathType(ing)
		return ing, true
	}

	return nil, false
}

// Run initiates the synchronization of the informers against the API server.
func (i *Informer) Run(stopCh chan struct{}) {
	// 监听的5个对象的事件
	go i.Secret.Run(stopCh)
	go i.Endpoint.Run(stopCh)
	if i.IngressClass != nil {
		go i.IngressClass.Run(stopCh)
	}
	go i.Service.Run(stopCh)
	go i.ConfigMap.Run(stopCh)

	// wait for all involved caches to be synced before processing items
	// from the queue
	if !cache.WaitForCacheSync(stopCh,
		i.Endpoint.HasSynced,
		i.Service.HasSynced,
		i.Secret.HasSynced,
		i.ConfigMap.HasSynced,
	) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
	}
	if i.IngressClass != nil && !cache.WaitForCacheSync(stopCh, i.IngressClass.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for ingress classcaches to sync"))
	}

	// when limit controller scope to one namespace, skip sync namespaces at cluster scope
	if i.Namespace != nil {
		go i.Namespace.Run(stopCh)

		if !cache.WaitForCacheSync(stopCh, i.Namespace.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		}
	}

	// in big clusters, deltas can keep arriving even after HasSynced
	// functions have returned 'true'
	time.Sleep(1 * time.Second)

	// we can start syncing ingress objects only after other caches are
	// ready, because ingress rules require content from other listers, and
	// 'add' events get triggered in the handlers during caches population.
	go i.Ingress.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh,
		i.Ingress.HasSynced,
	) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
	}
}
