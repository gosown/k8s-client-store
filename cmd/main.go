package main

import (
	"github.com/eapache/channels"
	store "github.com/gosown/k8s-client-store"
	"github.com/gosown/k8s-client-store/task"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"net"
	"net/http"
	"sync"
	"time"
)

type Configuration struct {
	APIServerHost string
	RootCAFile    string

	KubeConfigFile string

	Client clientset.Interface

	ResyncPeriod time.Duration

	ConfigMapName  string
	DefaultService string

	Namespace string

	WatchNamespaceSelector labels.Selector

	// +optional
	TCPConfigMapName string
	// +optional
	UDPConfigMapName string

	DefaultSSLCertificate string

	// +optional
	PublishService       string
	PublishStatusAddress string

	UpdateStatus           bool
	UseNodeInternalIP      bool
	ElectionID             string
	UpdateStatusOnShutdown bool

	HealthCheckHost string

	DisableServiceExternalName bool

	EnableSSLPassthrough bool

	EnableProfiling bool

	EnableMetrics       bool
	MetricsPerHost      bool
	ReportStatusClasses bool

	SyncRateLimit float32

	DisableCatchAll bool

	ValidationWebhook         string
	ValidationWebhookCertPath string
	ValidationWebhookKeyPath  string
	DisableFullValidationTest bool

	MaxmindEditionFiles *[]string

	MonitorMaxBatchSize int

	PostShutdownGracePeriod int
	ShutdownGracePeriod     int

	InternalLoggerAddress string
	IsChroot              bool
	DeepInspector         bool

	DynamicConfigurationRetries int
}

type NGINXController struct {
	cfg *Configuration

	recorder record.EventRecorder

	syncQueue *task.Queue

	syncRateLimiter flowcontrol.RateLimiter

	// stopLock is used to enforce that only a single call to Stop send at
	// a given time. We allow stopping through an HTTP endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.Mutex

	stopCh   chan struct{}
	updateCh *channels.RingChannel

	// ngxErrCh is used to detect errors with the NGINX processes
	ngxErrCh chan error

	resolver []net.IP

	isIPV6Enabled bool

	isShuttingDown bool

	store store.Storer

	validationWebhookServer *http.Server
}

func (n *NGINXController) syncIngress(interface{}) error {
	n.syncRateLimiter.Accept()

	if n.syncQueue.IsShuttingDown() {
		return nil
	}
	return nil
}

func main() {
	config := &Configuration{}

	n := &NGINXController{}

	n.store = store.New(
		config.Namespace,
		config.WatchNamespaceSelector,
		config.ConfigMapName,
		config.TCPConfigMapName,
		config.UDPConfigMapName,
		config.DefaultSSLCertificate,
		config.ResyncPeriod,
		config.Client,
		n.updateCh, // 当有更新后，推到此channel
		config.DisableCatchAll,
		config.DeepInspector)

	n.syncQueue = task.NewTaskQueue(n.syncIngress) // 设置消费方
}
