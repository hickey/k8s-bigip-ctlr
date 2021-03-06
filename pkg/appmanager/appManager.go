/*-
 * Copyright (c) 2016,2017, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package appmanager

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
	"github.com/F5Networks/k8s-bigip-ctlr/pkg/writer"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	rest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	routeapi "github.com/openshift/origin/pkg/route/api"
)

const DefaultConfigMapLabel = "f5type in (virtual-server)"
const vsBindAddrAnnotation = "status.virtual-server.f5.com/ip"
const ingressSslRedirect = "ingress.kubernetes.io/ssl-redirect"
const ingressAllowHttp = "ingress.kubernetes.io/allow-http"
const ingHealthMonitorAnnotation = "virtual-server.f5.com/health"

type ResourceMap map[int32][]*ResourceConfig

type Manager struct {
	resources         *Resources
	customProfiles    CustomProfileStore
	irulesMap         IRulesMap
	intDgMap          InternalDataGroupMap
	kubeClient        kubernetes.Interface
	restClientv1      rest.Interface
	restClientv1beta1 rest.Interface
	routeClientV1     rest.Interface
	configWriter      writer.Writer
	initialState      bool
	// Use internal node IPs
	useNodeInternal bool
	// Running in nodeport (or cluster) mode
	isNodePort bool
	// Mutex to control access to node data
	// FIXME: Simple synchronization for now, it remains to be determined if we'll
	// need something more complicated (channels, etc?)
	oldNodesMutex sync.Mutex
	// Nodes from previous iteration of node polling
	oldNodes []string
	// Mutex for all informers (for informer CRUD)
	informersMutex sync.Mutex
	// Mutex for irulesMap
	irulesMutex sync.Mutex
	// Mutex for intDgMap
	intDgMutex sync.Mutex
	// App informer support
	vsQueue      workqueue.RateLimitingInterface
	appInformers map[string]*appInformer
	// Namespace informer support (namespace labels)
	nsQueue    workqueue.RateLimitingInterface
	nsInformer cache.SharedIndexInformer
	// Event recorder
	broadcaster   record.EventBroadcaster
	eventRecorder record.EventRecorder
	eventSource   v1.EventSource
	// Route configurations
	routeConfig RouteConfig
}

// Struct to allow NewManager to receive all or only specific parameters.
type Params struct {
	KubeClient      kubernetes.Interface
	restClient      rest.Interface // package local for unit testing only
	RouteClientV1   rest.Interface
	ConfigWriter    writer.Writer
	UseNodeInternal bool
	IsNodePort      bool
	RouteConfig     RouteConfig
	InitialState    bool                 // Unit testing only
	EventRecorder   record.EventRecorder // Unit testing only
}

// Configuration options for Routes in OpenShift
type RouteConfig struct {
	RouteVSAddr string
	RouteLabel  string
}

// Create and return a new app manager that meets the Manager interface
func NewManager(params *Params) *Manager {
	vsQueue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(), "virtual-server-controller")
	nsQueue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(), "namespace-controller")
	manager := Manager{
		resources:         NewResources(),
		customProfiles:    NewCustomProfiles(),
		irulesMap:         make(IRulesMap),
		intDgMap:          make(InternalDataGroupMap),
		kubeClient:        params.KubeClient,
		restClientv1:      params.restClient,
		restClientv1beta1: params.restClient,
		routeClientV1:     params.RouteClientV1,
		configWriter:      params.ConfigWriter,
		useNodeInternal:   params.UseNodeInternal,
		isNodePort:        params.IsNodePort,
		initialState:      params.InitialState,
		eventRecorder:     params.EventRecorder,
		routeConfig:       params.RouteConfig,
		vsQueue:           vsQueue,
		nsQueue:           nsQueue,
		appInformers:      make(map[string]*appInformer),
	}
	if nil != manager.kubeClient && nil == manager.restClientv1 {
		// This is the normal production case, but need the checks for unit tests.
		manager.restClientv1 = manager.kubeClient.Core().RESTClient()
	}
	if nil != manager.kubeClient && nil == manager.restClientv1beta1 {
		// This is the normal production case, but need the checks for unit tests.
		manager.restClientv1beta1 = manager.kubeClient.Extensions().RESTClient()
	}
	manager.eventSource = v1.EventSource{Component: "k8s-bigip-ctlr"}
	manager.broadcaster = record.NewBroadcaster()
	if nil == manager.eventRecorder {
		manager.eventRecorder = manager.broadcaster.NewRecorder(scheme.Scheme, manager.eventSource)
	}

	return &manager
}

func (appMgr *Manager) loadDefaultCert(
	namespace string,
) (*ProfileRef, bool) {
	// OpenShift will put the default server SSL cert on each pod. We create a
	// server SSL profile for it and associate it to any reencrypt routes that
	// have not explicitly set a certificate.
	profileName := "openshift_route_cluster_default-server-ssl"
	profile := ProfileRef{
		Name:      profileName,
		Partition: DEFAULT_PARTITION,
		Context:   customProfileServer,
	}
	appMgr.customProfiles.Lock()
	defer appMgr.customProfiles.Unlock()
	skey := secretKey{Name: profileName, Namespace: namespace}
	_, found := appMgr.customProfiles.profs[skey]
	if !found {
		path := "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
		data, err := ioutil.ReadFile(path)
		if nil != err {
			log.Errorf("Unable to load default cluster certificate '%v': %v",
				path, err)
			return nil, false
		}
		appMgr.customProfiles.profs[skey] = NewCustomProfile(profile, string(data))
	}
	return &profile, !found
}

func (appMgr *Manager) addIRule(name, partition, rule string) {
	appMgr.irulesMutex.Lock()
	defer appMgr.irulesMutex.Unlock()

	key := nameRef{
		Name:      name,
		Partition: partition,
	}
	appMgr.irulesMap[key] = NewIRule(name, partition, rule)
}

func (appMgr *Manager) addInternalDataGroup(name, partition string) {
	appMgr.intDgMutex.Lock()
	defer appMgr.intDgMutex.Unlock()

	key := nameRef{
		Name:      name,
		Partition: partition,
	}
	appMgr.intDgMap[key] = NewInternalDataGroup(name, partition)
}

func (appMgr *Manager) watchingAllNamespacesLocked() bool {
	if 0 == len(appMgr.appInformers) {
		// Not watching any namespaces.
		return false
	}
	_, watchingAll := appMgr.appInformers[""]
	return watchingAll
}

func (appMgr *Manager) AddNamespace(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	_, err := appMgr.addNamespaceLocked(namespace, cfgMapSelector, resyncPeriod)
	return err
}

func (appMgr *Manager) addNamespaceLocked(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) (*appInformer, error) {
	if appMgr.watchingAllNamespacesLocked() {
		return nil, fmt.Errorf(
			"Cannot add additional namespaces when already watching all.")
	}
	if len(appMgr.appInformers) > 0 && "" == namespace {
		return nil, fmt.Errorf(
			"Cannot watch all namespaces when already watching specific ones.")
	}
	var appInf *appInformer
	if appInf, found := appMgr.appInformers[namespace]; found {
		return appInf, nil
	}
	appInf = appMgr.newAppInformer(namespace, cfgMapSelector, resyncPeriod)
	appMgr.appInformers[namespace] = appInf
	return appInf, nil
}

func (appMgr *Manager) removeNamespace(namespace string) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	err := appMgr.removeNamespaceLocked(namespace)
	return err
}

func (appMgr *Manager) removeNamespaceLocked(namespace string) error {
	if _, found := appMgr.appInformers[namespace]; !found {
		return fmt.Errorf("No informers exist for namespace %v\n", namespace)
	}
	delete(appMgr.appInformers, namespace)
	return nil
}

func (appMgr *Manager) AddNamespaceLabelInformer(
	labelSelector labels.Selector,
	resyncPeriod time.Duration,
) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	if nil != appMgr.nsInformer {
		return fmt.Errorf("Already have a namespace label informer added.")
	}
	if 0 != len(appMgr.appInformers) {
		return fmt.Errorf("Cannot set a namespace label informer when informers " +
			"have been setup for one or more namespaces.")
	}
	appMgr.nsInformer = cache.NewSharedIndexInformer(
		newListWatchWithLabelSelector(
			appMgr.restClientv1,
			"namespaces",
			"",
			labelSelector,
		),
		&v1.Namespace{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	appMgr.nsInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueNamespace(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueNamespace(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueNamespace(obj) },
		},
		resyncPeriod,
	)

	return nil
}

func (appMgr *Manager) enqueueNamespace(obj interface{}) {
	ns := obj.(*v1.Namespace)
	appMgr.nsQueue.Add(ns.ObjectMeta.Name)
}

func (appMgr *Manager) namespaceWorker() {
	for appMgr.processNextNamespace() {
	}
}

func (appMgr *Manager) processNextNamespace() bool {
	key, quit := appMgr.nsQueue.Get()
	if quit {
		return false
	}
	defer appMgr.nsQueue.Done(key)

	err := appMgr.syncNamespace(key.(string))
	if err == nil {
		appMgr.nsQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
	appMgr.nsQueue.AddRateLimited(key)

	return true
}

func (appMgr *Manager) syncNamespace(nsName string) error {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing namespace %+v (%v)",
			nsName, endTime.Sub(startTime))
	}()
	_, exists, err := appMgr.nsInformer.GetIndexer().GetByKey(nsName)
	if nil != err {
		log.Warningf("Error looking up namespace '%v': %v\n", nsName, err)
		return err
	}

	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appInf, found := appMgr.getNamespaceInformerLocked(nsName)
	if exists && found {
		return nil
	}
	if exists {
		// exists but not found in informers map, add
		cfgMapSelector, err := labels.Parse(DefaultConfigMapLabel)
		if err != nil {
			return fmt.Errorf("Failed to parse Label Selector string: %v", err)
		}
		appInf, err = appMgr.addNamespaceLocked(nsName, cfgMapSelector, 0)
		if err != nil {
			return fmt.Errorf("Failed to add informers for namespace %v: %v",
				nsName, err)
		}
		appInf.start()
		appInf.waitForCacheSync()
	} else {
		// does not exist but found in informers map, delete
		// Clean up all resources that reference a removed namespace
		appInf.stopInformers()
		appMgr.removeNamespaceLocked(nsName)
		appMgr.resources.Lock()
		defer appMgr.resources.Unlock()
		rsDeleted := 0
		appMgr.resources.ForEach(func(key serviceKey, cfg *ResourceConfig) {
			if key.Namespace == nsName {
				if appMgr.resources.Delete(key, "") {
					rsDeleted += 1
				}
			}
		})
		if rsDeleted > 0 {
			appMgr.outputConfigLocked()
		}
	}

	return nil
}

func (appMgr *Manager) GetWatchedNamespaces() []string {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	var namespaces []string
	for k, _ := range appMgr.appInformers {
		namespaces = append(namespaces, k)
	}
	return namespaces
}

func (appMgr *Manager) GetNamespaceLabelInformer() cache.SharedIndexInformer {
	return appMgr.nsInformer
}

type serviceQueueKey struct {
	Namespace   string
	ServiceName string
}

type appInformer struct {
	namespace      string
	cfgMapInformer cache.SharedIndexInformer
	svcInformer    cache.SharedIndexInformer
	endptInformer  cache.SharedIndexInformer
	ingInformer    cache.SharedIndexInformer
	routeInformer  cache.SharedIndexInformer
	stopCh         chan struct{}
}

func (appMgr *Manager) newAppInformer(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) *appInformer {
	appInf := appInformer{
		namespace: namespace,
		stopCh:    make(chan struct{}),
		cfgMapInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"configmaps",
				namespace,
				cfgMapSelector,
			),
			&v1.ConfigMap{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		svcInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"services",
				namespace,
				labels.Everything(),
			),
			&v1.Service{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		endptInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"endpoints",
				namespace,
				labels.Everything(),
			),
			&v1.Endpoints{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		ingInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1beta1,
				"ingresses",
				namespace,
				labels.Everything(),
			),
			&v1beta1.Ingress{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
	}
	if nil != appMgr.routeClientV1 {
		var label labels.Selector
		var err error
		if len(appMgr.routeConfig.RouteLabel) == 0 {
			label = labels.Everything()
		} else {
			label, err = labels.Parse(appMgr.routeConfig.RouteLabel)
			if err != nil {
				log.Errorf("Failed to parse Label Selector string: %v", err)
			}
		}
		appInf.routeInformer = cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.routeClientV1,
				"routes",
				namespace,
				label,
			),
			&routeapi.Route{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		)
	}

	appInf.cfgMapInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueConfigMap(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueConfigMap(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueConfigMap(obj) },
		},
		resyncPeriod,
	)

	appInf.svcInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueService(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueService(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueService(obj) },
		},
		resyncPeriod,
	)

	appInf.endptInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueEndpoints(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueEndpoints(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueEndpoints(obj) },
		},
		resyncPeriod,
	)

	appInf.ingInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueIngress(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueIngress(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueIngress(obj) },
		},
		resyncPeriod,
	)

	if nil != appMgr.routeClientV1 {
		appInf.routeInformer.AddEventHandlerWithResyncPeriod(
			&cache.ResourceEventHandlerFuncs{
				AddFunc:    func(obj interface{}) { appMgr.enqueueRoute(obj) },
				UpdateFunc: func(old, cur interface{}) { appMgr.enqueueRoute(cur) },
				DeleteFunc: func(obj interface{}) { appMgr.enqueueRoute(obj) },
			},
			resyncPeriod,
		)
	}

	return &appInf
}

func newListWatchWithLabelSelector(
	c cache.Getter,
	resource string,
	namespace string,
	labelSelector labels.Selector,
) cache.ListerWatcher {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			LabelsSelectorParam(labelSelector).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		return c.Get().
			Prefix("watch").
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			LabelsSelectorParam(labelSelector).
			Watch()
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func (appMgr *Manager) enqueueConfigMap(obj interface{}) {
	if ok, keys := appMgr.checkValidConfigMap(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueService(obj interface{}) {
	if ok, keys := appMgr.checkValidService(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueEndpoints(obj interface{}) {
	if ok, keys := appMgr.checkValidEndpoints(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueIngress(obj interface{}) {
	if ok, keys := appMgr.checkValidIngress(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueRoute(obj interface{}) {
	if ok, key := appMgr.checkValidRoute(obj); ok {
		appMgr.vsQueue.Add(*key)
	}
}

func (appMgr *Manager) getNamespaceInformer(
	ns string,
) (*appInformer, bool) {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appInf, found := appMgr.getNamespaceInformerLocked(ns)
	return appInf, found
}

func (appMgr *Manager) getNamespaceInformerLocked(
	ns string,
) (*appInformer, bool) {
	toFind := ns
	if appMgr.watchingAllNamespacesLocked() {
		toFind = ""
	}
	appInf, found := appMgr.appInformers[toFind]
	return appInf, found
}

func (appInf *appInformer) start() {
	go appInf.cfgMapInformer.Run(appInf.stopCh)
	go appInf.svcInformer.Run(appInf.stopCh)
	go appInf.endptInformer.Run(appInf.stopCh)
	go appInf.ingInformer.Run(appInf.stopCh)
	if nil != appInf.routeInformer {
		go appInf.routeInformer.Run(appInf.stopCh)
	}
}

func (appInf *appInformer) waitForCacheSync() {
	if nil != appInf.routeInformer {
		cache.WaitForCacheSync(
			appInf.stopCh,
			appInf.cfgMapInformer.HasSynced,
			appInf.svcInformer.HasSynced,
			appInf.endptInformer.HasSynced,
			appInf.ingInformer.HasSynced,
			appInf.routeInformer.HasSynced,
		)
	} else {
		cache.WaitForCacheSync(
			appInf.stopCh,
			appInf.cfgMapInformer.HasSynced,
			appInf.svcInformer.HasSynced,
			appInf.endptInformer.HasSynced,
			appInf.ingInformer.HasSynced,
		)
	}
}

func (appInf *appInformer) stopInformers() {
	close(appInf.stopCh)
}

func (appMgr *Manager) IsNodePort() bool {
	return appMgr.isNodePort
}

func (appMgr *Manager) UseNodeInternal() bool {
	return appMgr.useNodeInternal
}

func (appMgr *Manager) ConfigWriter() writer.Writer {
	return appMgr.configWriter
}

func (appMgr *Manager) Run(stopCh <-chan struct{}) {
	go appMgr.runImpl(stopCh)
}

func (appMgr *Manager) runImpl(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer appMgr.vsQueue.ShutDown()
	defer appMgr.nsQueue.ShutDown()

	appMgr.addIRule(httpRedirectIRuleName, DEFAULT_PARTITION,
		httpRedirectIRule(DEFAULT_HTTPS_PORT))

	if nil != appMgr.routeClientV1 {
		appMgr.addIRule(
			sslPassthroughIRuleName, DEFAULT_PARTITION, sslPassthroughIRule())
		appMgr.addInternalDataGroup(passthroughHostsDgName, DEFAULT_PARTITION)
		appMgr.addInternalDataGroup(reencryptHostsDgName, DEFAULT_PARTITION)
	}

	if nil != appMgr.nsInformer {
		// Using one worker for namespace label changes.
		appMgr.startAndSyncNamespaceInformer(stopCh)
		go wait.Until(appMgr.namespaceWorker, time.Second, stopCh)
	}

	appMgr.startAndSyncAppInformers()

	// Using only one virtual server worker currently.
	go wait.Until(appMgr.virtualServerWorker, time.Second, stopCh)

	<-stopCh
	appMgr.stopAppInformers()
}

func (appMgr *Manager) startAndSyncNamespaceInformer(stopCh <-chan struct{}) {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	go appMgr.nsInformer.Run(stopCh)
	cache.WaitForCacheSync(stopCh, appMgr.nsInformer.HasSynced)
}

func (appMgr *Manager) startAndSyncAppInformers() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appMgr.startAppInformersLocked()
	appMgr.waitForCacheSyncLocked()
}

func (appMgr *Manager) startAppInformersLocked() {
	for _, appInf := range appMgr.appInformers {
		appInf.start()
	}
}

func (appMgr *Manager) waitForCacheSync() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appMgr.waitForCacheSyncLocked()
}

func (appMgr *Manager) waitForCacheSyncLocked() {
	for _, appInf := range appMgr.appInformers {
		appInf.waitForCacheSync()
	}
}

func (appMgr *Manager) stopAppInformers() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	for _, appInf := range appMgr.appInformers {
		appInf.stopInformers()
	}
}

func (appMgr *Manager) virtualServerWorker() {
	for appMgr.processNextVirtualServer() {
	}
}

func (appMgr *Manager) processNextVirtualServer() bool {
	key, quit := appMgr.vsQueue.Get()
	if quit {
		// The controller is shutting down.
		return false
	}
	defer appMgr.vsQueue.Done(key)

	err := appMgr.syncVirtualServer(key.(serviceQueueKey))
	if err == nil {
		appMgr.vsQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
	appMgr.vsQueue.AddRateLimited(key)

	return true
}

type vsSyncStats struct {
	vsFound   int
	vsUpdated int
	vsDeleted int
	cpUpdated int
	dgUpdated int
}

func (appMgr *Manager) syncVirtualServer(sKey serviceQueueKey) error {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing virtual servers %+v (%v)",
			sKey, endTime.Sub(startTime))
	}()

	// Get the informers for the namespace. This will tell us if we care about
	// this item.
	appInf, haveNamespace := appMgr.getNamespaceInformer(sKey.Namespace)
	if !haveNamespace {
		// This shouldn't happen as the namespace is checked for every item before
		// it is added to the queue, but issue a warning if it does.
		log.Warningf(
			"Received an update for an item from an un-watched namespace %v",
			sKey.Namespace)
		return nil
	}

	// Lookup the service
	svcKey := sKey.Namespace + "/" + sKey.ServiceName
	obj, svcFound, err := appInf.svcInformer.GetIndexer().GetByKey(svcKey)
	if nil != err {
		// Returning non-nil err will re-queue this item with rate-limiting.
		log.Warningf("Error looking up service '%v': %v\n", svcKey, err)
		return err
	}

	// Use a map to allow ports in the service to be looked up quickly while
	// looping through the ConfigMaps. The value is not currently used.
	svcPortMap := make(map[int32]bool)
	var svc *v1.Service
	if svcFound {
		svc = obj.(*v1.Service)
		for _, portSpec := range svc.Spec.Ports {
			svcPortMap[portSpec.Port] = false
		}
	}

	// rsMap stores all resources currently in Resources matching sKey, indexed by port
	rsMap := appMgr.getResourcesForKey(sKey)

	var stats vsSyncStats
	err = appMgr.syncConfigMaps(&stats, sKey, rsMap, svcPortMap, svc, appInf)
	if nil != err {
		return err
	}

	err = appMgr.syncIngresses(&stats, sKey, rsMap, svcPortMap, svc, appInf)
	if nil != err {
		return err
	}
	if nil != appInf.routeInformer {
		err = appMgr.syncRoutes(&stats, sKey, rsMap, svcPortMap, svc, appInf)
		if nil != err {
			return err
		}
	}

	if len(rsMap) > 0 {
		// We get here when there are ports defined in the service that don't
		// have a corresponding config map.
		stats.vsDeleted = appMgr.deleteUnusedResources(sKey, rsMap)
		appMgr.deleteUnusedRoutes(sKey.Namespace)
	}
	log.Debugf("Updated %v of %v virtual server configs, deleted %v",
		stats.vsUpdated, stats.vsFound, stats.vsDeleted)

	// delete any custom profiles that are no longer referenced
	appMgr.deleteUnusedProfiles(sKey.Namespace)

	if stats.vsUpdated > 0 || stats.vsDeleted > 0 || stats.cpUpdated > 0 ||
		stats.dgUpdated > 0 {
		appMgr.outputConfig()
	} else if appMgr.vsQueue.Len() == 0 && appMgr.nsQueue.Len() == 0 {
		appMgr.resources.Lock()
		defer appMgr.resources.Unlock()
		if !appMgr.initialState {
			appMgr.outputConfigLocked()
		}
	}

	return nil
}

func (appMgr *Manager) syncConfigMaps(
	stats *vsSyncStats,
	sKey serviceQueueKey,
	rsMap ResourceMap,
	svcPortMap map[int32]bool,
	svc *v1.Service,
	appInf *appInformer,
) error {
	cfgMapsByIndex, err := appInf.cfgMapInformer.GetIndexer().ByIndex(
		"namespace", sKey.Namespace)
	if nil != err {
		log.Warningf("Unable to list config maps for namespace '%v': %v",
			sKey.Namespace, err)
		return err
	}
	for _, obj := range cfgMapsByIndex {
		// We need to look at all config maps in the store, parse the data blob,
		// and see if it belongs to the service that has changed.
		cm := obj.(*v1.ConfigMap)
		if cm.ObjectMeta.Namespace != sKey.Namespace {
			continue
		}
		rsCfg, err := parseConfigMap(cm)
		if nil != err {
			// Ignore this config map for the time being. When the user updates it
			// so that it is valid it will be requeued.
			fmt.Errorf("Error parsing ConfigMap %v_%v",
				cm.ObjectMeta.Namespace, cm.ObjectMeta.Name)
			continue
		}

		// Check if SSLProfile(s) are contained in Secrets
		for _, profile := range rsCfg.Virtual.GetFrontendSslProfileNames() {
			// Check if profile is contained in a Secret
			secret, err := appMgr.kubeClient.Core().Secrets(cm.ObjectMeta.Namespace).
				Get(profile, metav1.GetOptions{})
			if err != nil {
				// No secret, so we assume the profile is a BIG-IP default
				log.Infof("Couldn't find Secret with name '%s', parsing secretName as path.",
					profile)
				continue
			}
			err, updated := appMgr.handleSslProfile(rsCfg, secret, cm.ObjectMeta.Namespace)
			if err != nil {
				log.Warningf("%v", err)
				continue
			}
			if updated {
				stats.cpUpdated += 1
			}
			// Replace the current stored sslProfile with a correctly formatted
			// profile (since this profile is just a secret name)
			rsCfg.Virtual.RemoveFrontendSslProfileName(profile)
			secretName := formatIngressSslProfileName(
				rsCfg.Virtual.Partition + "/" + profile)
			rsCfg.Virtual.AddFrontendSslProfileName(secretName)
		}

		rsName := rsCfg.Virtual.VirtualServerName
		if ok, found, updated := appMgr.handleConfigForType(
			rsCfg, sKey, rsMap, rsName, svcPortMap, svc, appInf, ""); !ok {
			stats.vsUpdated += updated
			continue
		} else {
			stats.vsFound += found
			stats.vsUpdated += updated
		}

		// Set a status annotation to contain the virtualAddress bindAddr
		if rsCfg.Virtual.IApp == "" &&
			rsCfg.Virtual.VirtualAddress != nil &&
			rsCfg.Virtual.VirtualAddress.BindAddr != "" {
			appMgr.setBindAddrAnnotation(cm, sKey, rsCfg)
		}
	}
	return nil
}

func (appMgr *Manager) syncIngresses(
	stats *vsSyncStats,
	sKey serviceQueueKey,
	rsMap ResourceMap,
	svcPortMap map[int32]bool,
	svc *v1.Service,
	appInf *appInformer,
) error {
	ingByIndex, err := appInf.ingInformer.GetIndexer().ByIndex(
		"namespace", sKey.Namespace)
	if nil != err {
		log.Warningf("Unable to list ingresses for namespace '%v': %v",
			sKey.Namespace, err)
		return err
	}
	for _, obj := range ingByIndex {
		// We need to look at all ingresses in the store, parse the data blob,
		// and see if it belongs to the service that has changed.
		ing := obj.(*v1beta1.Ingress)
		if ing.ObjectMeta.Namespace != sKey.Namespace {
			continue
		}

		for _, portStruct := range appMgr.virtualPorts(ing) {
			rsCfg := createRSConfigFromIngress(ing, sKey.Namespace,
				appInf.svcInformer.GetIndexer(), portStruct)
			if rsCfg == nil {
				// Currently, an error is returned only if the Ingress is one we
				// do not care about
				continue
			}

			// Handle TLS configuration
			updated := appMgr.handleIngressTls(rsCfg, ing)
			if updated {
				stats.cpUpdated += 1
			}

			// Handle Ingress health monitors
			rsName := rsCfg.Virtual.VirtualServerName
			hmStr, found := ing.ObjectMeta.Annotations[ingHealthMonitorAnnotation]
			if found {
				var monitors IngressHealthMonitors
				err := json.Unmarshal([]byte(hmStr), &monitors)
				if err != nil {
					msg := fmt.Sprintf(
						"Unable to parse health monitor JSON array '%v': %v", hmStr, err)
					log.Errorf("%s", msg)
					appMgr.recordIngressEvent(ing, "InvalidData", msg, rsName)
				} else {
					if nil != ing.Spec.Backend {
						appMgr.handleSingleServiceHealthMonitors(
							rsName, rsCfg, ing, monitors)
					} else {
						appMgr.handleMultiServiceHealthMonitors(
							rsName, rsCfg, ing, monitors)
					}
				}
				rsCfg.SortMonitors()
			}

			// make sure all policies across configs for this Ingress match each other
			appMgr.resources.Lock()
			cfgs, keys := appMgr.resources.GetAllWithName(rsCfg.Virtual.VirtualServerName)

			for i, cfg := range cfgs {
				for _, policy := range rsCfg.Policies {
					if policy.Name == rsCfg.Virtual.VirtualServerName {
						cfg.SetPolicy(policy)
					}
				}
				appMgr.resources.Assign(keys[i], rsCfg.Virtual.VirtualServerName, cfg)
			}
			appMgr.resources.Unlock()

			if ok, found, updated := appMgr.handleConfigForType(
				rsCfg, sKey, rsMap, rsName, svcPortMap, svc, appInf, ""); !ok {
				stats.vsUpdated += updated
				continue
			} else {
				if updated > 0 && !appMgr.processAllMultiSvc(len(rsCfg.Pools),
					rsCfg.Virtual.VirtualServerName) {
					updated -= 1
				}
				stats.vsFound += found
				stats.vsUpdated += updated
				if updated > 0 {
					msg := fmt.Sprintf(
						"Created a ResourceConfig '%v' for the Ingress.",
						rsCfg.Virtual.VirtualServerName)
					appMgr.recordIngressEvent(ing, "ResourceConfigured", msg, "")
				}
			}
			// Set the Ingress Status IP address
			appMgr.setIngressStatus(ing, rsCfg)
		}
	}
	return nil
}

func (appMgr *Manager) syncRoutes(
	stats *vsSyncStats,
	sKey serviceQueueKey,
	rsMap ResourceMap,
	svcPortMap map[int32]bool,
	svc *v1.Service,
	appInf *appInformer,
) error {
	routeByIndex, err := appInf.getOrderedRoutes(sKey.Namespace)
	if nil != err {
		log.Warningf("Unable to list routes for namespace '%v': %v",
			sKey.Namespace, err)
		return err
	}

	// Rebuild all internal data groups for routes as we process each
	dgMap := make(InternalDataGroupMap)
	for _, route := range routeByIndex {
		// We need to look at all routes in the store, parse the data blob,
		// and see if it belongs to the service that has changed.
		if nil != route.Spec.TLS {
			// The information stored in the internal data groups can span multiple
			// namespaces, so we need to keep them updated with all current routes
			// regardless of anything that happens below.
			switch route.Spec.TLS.Termination {
			case routeapi.TLSTerminationPassthrough:
				updateDataGroupForPassthroughRoute(route, DEFAULT_PARTITION, dgMap)
			case routeapi.TLSTerminationReencrypt:
				updateDataGroupForReencryptRoute(route, DEFAULT_PARTITION, dgMap)
			}
		}
		if route.ObjectMeta.Namespace != sKey.Namespace {
			continue
		}
		pStructs := []portStruct{{protocol: "http", port: DEFAULT_HTTP_PORT},
			{protocol: "https", port: DEFAULT_HTTPS_PORT}}
		for _, ps := range pStructs {
			rsCfg, err := createRSConfigFromRoute(route,
				*appMgr.resources, appMgr.routeConfig, ps)
			if err != nil {
				// We return err if there was an error creating a rule
				log.Warningf("%v", err)
				continue
			}

			rsName := rsCfg.Virtual.VirtualServerName
			if ok, found, updated := appMgr.handleConfigForType(
				&rsCfg, sKey, rsMap, rsName, svcPortMap, svc, appInf,
				route.Spec.To.Name); !ok {
				stats.vsUpdated += updated
				continue
			} else {
				stats.vsFound += found
				stats.vsUpdated += updated
			}

			// We store this same config on every route that has the same protocol, but it is only
			// written to the BIG-IP once. This block guarantees that all of these configs
			// are in the same state.
			appMgr.resources.Lock()
			cfgs, keys := appMgr.resources.GetAllWithName(rsCfg.Virtual.VirtualServerName)
			for i, cfg := range cfgs {
				if cfg.Virtual.Partition == rsCfg.Virtual.Partition &&
					!reflect.DeepEqual(cfg, rsCfg) {
					cfg = &rsCfg
					appMgr.resources.Assign(keys[i], cfg.Virtual.VirtualServerName, cfg)
				}
			}
			appMgr.resources.Unlock()

			// TLS Cert/Key
			if nil != route.Spec.TLS &&
				rsCfg.Virtual.VirtualAddress.Port == DEFAULT_HTTPS_PORT {
				switch route.Spec.TLS.Termination {
				case routeapi.TLSTerminationEdge:
					appMgr.setClientSslProfile(stats, sKey, &rsCfg, route)
				case routeapi.TLSTerminationReencrypt:
					appMgr.setClientSslProfile(stats, sKey, &rsCfg, route)
					appMgr.setServerSslProfile(stats, sKey, &rsCfg, route)
				}
			}
		}
	}

	// Update internal data groups for routes if changed
	appMgr.updateRouteDataGroups(stats, dgMap)

	return nil
}

func (appMgr *Manager) setClientSslProfile(
	stats *vsSyncStats,
	sKey serviceQueueKey,
	rsCfg *ResourceConfig,
	route *routeapi.Route,
) {
	profileName := "Common/clientssl"
	if "" != route.Spec.TLS.Certificate && "" != route.Spec.TLS.Key {
		cp := CustomProfile{
			Name:       route.ObjectMeta.Name + "-https-cert",
			Partition:  rsCfg.Virtual.Partition,
			Context:    customProfileClient,
			Cert:       route.Spec.TLS.Certificate,
			Key:        route.Spec.TLS.Key,
			ServerName: route.Spec.Host,
		}
		skey := secretKey{
			Name:         cp.Name,
			Namespace:    sKey.Namespace,
			ResourceName: rsCfg.Virtual.VirtualServerName,
		}
		appMgr.customProfiles.Lock()
		defer appMgr.customProfiles.Unlock()
		if prof, ok := appMgr.customProfiles.profs[skey]; ok {
			if !reflect.DeepEqual(prof, cp) {
				stats.cpUpdated += 1
			}
		}
		appMgr.customProfiles.profs[skey] = cp
		profileName = fmt.Sprintf("%s/%s", cp.Partition, cp.Name)
	}
	rsCfg.Virtual.AddFrontendSslProfileName(profileName)
}

func (appMgr *Manager) setServerSslProfile(
	stats *vsSyncStats,
	sKey serviceQueueKey,
	rsCfg *ResourceConfig,
	route *routeapi.Route,
) {
	if "" != route.Spec.TLS.DestinationCACertificate {
		// Create new SSL server profile with the provided CA Certificate.
		ruleName := formatRouteRuleName(route)
		profile := ProfileRef{
			Name:      ruleName + "-server-ssl",
			Partition: rsCfg.Virtual.Partition,
			Context:   customProfileServer,
		}
		cp := NewCustomProfile(profile, route.Spec.TLS.DestinationCACertificate)
		skey := secretKey{
			Name:         cp.Name,
			Namespace:    sKey.Namespace,
			ResourceName: rsCfg.Virtual.VirtualServerName,
		}
		appMgr.customProfiles.Lock()
		defer appMgr.customProfiles.Unlock()
		if prof, ok := appMgr.customProfiles.profs[skey]; ok {
			if !reflect.DeepEqual(prof, cp) {
				stats.cpUpdated += 1
			}
		}
		appMgr.customProfiles.profs[skey] = cp
		rsCfg.Virtual.AddOrUpdateProfile(profile)
	} else {
		profile, added := appMgr.loadDefaultCert(sKey.Namespace)
		if nil != profile {
			rsCfg.Virtual.AddOrUpdateProfile(*profile)
		}
		if added {
			stats.cpUpdated += 1
		}
	}
}

func getBooleanAnnotation(
	annotations map[string]string,
	key string,
	defaultValue bool,
) bool {
	val, found := annotations[key]
	if !found {
		return defaultValue
	}
	bVal, err := strconv.ParseBool(val)
	if nil != err {
		log.Errorf("Unable to parse boolean value '%v': %v", val, err)
		return defaultValue
	}
	return bVal
}

type secretKey struct {
	Name         string
	Namespace    string
	ResourceName string
}

// Return value is whether or not a custom profile was updated
func (appMgr *Manager) handleIngressTls(
	rsCfg *ResourceConfig,
	ing *v1beta1.Ingress,
) bool {
	if 0 == len(ing.Spec.TLS) {
		// Nothing to do if no TLS section
		return false
	}
	if nil == rsCfg.Virtual.VirtualAddress ||
		rsCfg.Virtual.VirtualAddress.BindAddr == "" {
		// Nothing to do for pool-only mode
		return false
	}

	var httpsPort int32
	if port, ok :=
		ing.ObjectMeta.Annotations["virtual-server.f5.com/https-port"]; ok == true {
		p, _ := strconv.ParseInt(port, 10, 32)
		httpsPort = int32(p)
	} else {
		httpsPort = DEFAULT_HTTPS_PORT
	}
	// If we are processing the HTTPS server,
	// then we don't need a redirect policy, only profiles
	if rsCfg.Virtual.VirtualAddress.Port == httpsPort {
		var cpUpdated, updateState bool
		for _, tls := range ing.Spec.TLS {
			// Check if profile is contained in a Secret
			secret, err := appMgr.kubeClient.Core().Secrets(ing.ObjectMeta.Namespace).
				Get(tls.SecretName, metav1.GetOptions{})
			if err != nil {
				// No secret, so we assume the profile is a BIG-IP default
				log.Infof("Couldn't find Secret with name '%s': %s. Parsing secretName as path.",
					tls.SecretName, err)
				secretName := formatIngressSslProfileName(tls.SecretName)
				rsCfg.Virtual.AddFrontendSslProfileName(secretName)
				continue
			}
			err, cpUpdated = appMgr.handleSslProfile(rsCfg, secret, ing.ObjectMeta.Namespace)
			if err != nil {
				log.Warningf("%v", err)
				continue
			}
			updateState = updateState || cpUpdated
			secretName := formatIngressSslProfileName(
				rsCfg.Virtual.Partition + "/" + tls.SecretName)
			rsCfg.Virtual.AddFrontendSslProfileName(secretName)
		}
		return cpUpdated
	}

	// sslRedirect defaults to true, allowHttp defaults to false.
	sslRedirect := getBooleanAnnotation(ing.ObjectMeta.Annotations,
		ingressSslRedirect, true)
	allowHttp := getBooleanAnnotation(ing.ObjectMeta.Annotations,
		ingressAllowHttp, false)
	// -----------------------------------------------------------------
	// | State | sslRedirect | allowHttp | Description                 |
	// -----------------------------------------------------------------
	// |   1   |     F       |    F      | Just HTTPS, nothing on HTTP |
	// -----------------------------------------------------------------
	// |   2   |     T       |    F      | HTTP redirects to HTTPS     |
	// -----------------------------------------------------------------
	// |   2   |     T       |    T      | Honor sslRedirect == true   |
	// -----------------------------------------------------------------
	// |   3   |     F       |    T      | Both HTTP and HTTPS         |
	// -----------------------------------------------------------------
	var rule *Rule
	var policyName string
	if sslRedirect {
		// State 2, set HTTP redirect iRule
		log.Debugf("TLS: Applying HTTP redirect iRule.")
		ruleName := fmt.Sprintf("/%s/%s", DEFAULT_PARTITION, httpRedirectIRuleName)
		if httpsPort != DEFAULT_HTTPS_PORT {
			ruleName = fmt.Sprintf("%s_%d", ruleName, httpsPort)
			appMgr.addIRule(ruleName, DEFAULT_PARTITION,
				httpRedirectIRule(httpsPort))
		}
		rsCfg.Virtual.AddIRule(ruleName)
	} else if allowHttp {
		// State 3, do not apply any policy
		log.Debugf("TLS: Not applying any policies.")
	}

	if nil != rule && "" != policyName {
		policy := rsCfg.FindPolicy("forwarding")
		if nil == policy {
			policy = createPolicy(Rules{rule}, policyName, rsCfg.Virtual.Partition)
		} else {
			rule.Ordinal = len(policy.Rules)
			policy.Rules = append(policy.Rules, rule)
		}
		rsCfg.SetPolicy(*policy)
	}
	return false
}

func (appMgr *Manager) handleSslProfile(
	rsCfg *ResourceConfig,
	secret *v1.Secret,
	namespace string) (error, bool) {
	if _, ok := secret.Data["tls.crt"]; !ok {
		err := fmt.Errorf("Invalid Secret '%v': 'tls.crt' field not specified.",
			secret.ObjectMeta.Name)
		return err, false
	}
	if _, ok := secret.Data["tls.key"]; !ok {
		err := fmt.Errorf("Invalid Secret '%v': 'tls.key' field not specified.",
			secret.ObjectMeta.Name)
		return err, false
	}

	cp := CustomProfile{
		Name:      secret.ObjectMeta.Name,
		Partition: rsCfg.Virtual.Partition,
		Context:   customProfileClient,
		Cert:      string(secret.Data["tls.crt"]),
		Key:       string(secret.Data["tls.key"]),
	}
	skey := secretKey{
		Name:         cp.Name,
		Namespace:    namespace,
		ResourceName: rsCfg.Virtual.VirtualServerName,
	}
	appMgr.customProfiles.Lock()
	defer appMgr.customProfiles.Unlock()
	if prof, ok := appMgr.customProfiles.profs[skey]; ok {
		if !reflect.DeepEqual(prof, cp) {
			appMgr.customProfiles.profs[skey] = cp
			return nil, true
		} else {
			return nil, false
		}
	}
	appMgr.customProfiles.profs[skey] = cp
	return nil, false
}

type portStruct struct {
	protocol string
	port     int32
}

// Return the required ports for Ingress VS (depending on sslRedirect/allowHttp vals)
func (appMgr *Manager) virtualPorts(ing *v1beta1.Ingress) []portStruct {
	var httpPort int32
	var httpsPort int32
	if port, ok := ing.ObjectMeta.Annotations["virtual-server.f5.com/http-port"]; ok == true {
		p, _ := strconv.ParseInt(port, 10, 32)
		httpPort = int32(p)
	} else {
		httpPort = DEFAULT_HTTP_PORT
	}
	if port, ok := ing.ObjectMeta.Annotations["virtual-server.f5.com/https-port"]; ok == true {
		p, _ := strconv.ParseInt(port, 10, 32)
		httpsPort = int32(p)
	} else {
		httpsPort = DEFAULT_HTTPS_PORT
	}
	// sslRedirect defaults to true, allowHttp defaults to false.
	sslRedirect := getBooleanAnnotation(ing.ObjectMeta.Annotations,
		ingressSslRedirect, true)
	allowHttp := getBooleanAnnotation(ing.ObjectMeta.Annotations,
		ingressAllowHttp, false)

	http := portStruct{
		protocol: "http",
		port:     httpPort,
	}
	https := portStruct{
		protocol: "https",
		port:     httpsPort,
	}
	var ports []portStruct
	if len(ing.Spec.TLS) > 0 {
		if sslRedirect || allowHttp {
			// States 2,3; both HTTP and HTTPS
			// 2 virtual servers needed
			ports = append(ports, http)
			ports = append(ports, https)
		} else {
			// State 1; HTTPS only
			ports = append(ports, https)
		}
	} else {
		// HTTP only, no TLS
		ports = append(ports, http)
	}
	return ports
}

// Common handling function for ConfigMaps, Ingresses, and Routes
func (appMgr *Manager) handleConfigForType(
	rsCfg *ResourceConfig,
	sKey serviceQueueKey,
	rsMap ResourceMap,
	rsName string,
	svcPortMap map[int32]bool,
	svc *v1.Service,
	appInf *appInformer,
	currRouteSvc string, // Only used for Routes
) (bool, int, int) {
	vsFound := 0
	vsUpdated := 0

	var pool Pool
	found := false
	plIdx := 0
	for i, pl := range rsCfg.Pools {
		if pl.ServiceName == sKey.ServiceName {
			found = true
			pool = pl
			plIdx = i
			break
		}
	}
	if !found {
		// If the current cfg has no pool for this service,
		// remove any pools associated with the service,
		// across all stored keys for the resource
		appMgr.resources.Lock()
		defer appMgr.resources.Unlock()
		cfgs, keys := appMgr.resources.GetAllWithName(rsName)
		for i, cfg := range cfgs {
			for j, pool := range cfg.Pools {
				if pool.ServiceName == sKey.ServiceName {
					copy(cfg.Pools[j:], cfg.Pools[j+1:])
					cfg.Pools[len(cfg.Pools)-1] = Pool{}
					cfg.Pools = cfg.Pools[:len(cfg.Pools)-1]
				}
			}
			appMgr.resources.Assign(keys[i], rsName, cfg)
		}
		return false, vsFound, vsUpdated
	}
	svcKey := serviceKey{
		Namespace:   sKey.Namespace,
		ServiceName: pool.ServiceName,
		ServicePort: pool.ServicePort,
	}

	// Match, remove from rsMap so we don't delete it at the end.
	// In the case of Routes: If the svc of the currently processed route doesn't match
	// the svc in our serviceKey, then we don't want to delete it from the map (all routes
	// with the same protocol have the same VS name, so we don't want to ignore a route that
	// was actually deleted).
	cfgList := rsMap[pool.ServicePort]
	if currRouteSvc == "" || currRouteSvc == sKey.ServiceName {
		if len(cfgList) == 1 {
			delete(rsMap, pool.ServicePort)
		} else if len(cfgList) > 1 {
			for index, val := range cfgList {
				if val.Virtual.VirtualServerName == rsName {
					cfgList = append(cfgList[:index], cfgList[index+1:]...)
				}
			}
			rsMap[pool.ServicePort] = cfgList
		}
	}

	if _, ok := svcPortMap[pool.ServicePort]; !ok {
		log.Debugf("Process Service delete - name: %v namespace: %v",
			pool.ServiceName, svcKey.Namespace)
		log.Infof("Port '%v' for service '%v' was not found.",
			pool.ServicePort, pool.ServiceName)
		if appMgr.deactivateVirtualServer(svcKey, rsName, rsCfg, plIdx) {
			vsUpdated += 1
		}
	}

	if nil == svc {
		// The service is gone, de-activate it in the config.
		log.Infof("Service '%v' has not been found.", pool.ServiceName)
		if appMgr.deactivateVirtualServer(svcKey, rsName, rsCfg, plIdx) {
			vsUpdated += 1
		}

		// If this is an Ingress resource, add an event that the service wasn't found
		if strings.HasSuffix(rsName, "ingress") {
			msg := fmt.Sprintf("Service '%v' has not been found.",
				pool.ServiceName)
			appMgr.recordIngressEvent(nil, "ServiceNotFound", msg, rsName)
		}
		return false, vsFound, vsUpdated
	}

	// Update pool members.
	vsFound += 1
	correctBackend := true
	var reason string
	var msg string
	if appMgr.IsNodePort() {
		correctBackend, reason, msg =
			appMgr.updatePoolMembersForNodePort(svc, svcKey, rsCfg, plIdx)
	} else {
		correctBackend, reason, msg =
			appMgr.updatePoolMembersForCluster(svc, svcKey, rsCfg, appInf, plIdx)
	}

	// This will only update the config if the vs actually changed.
	if appMgr.saveVirtualServer(svcKey, rsName, rsCfg) {
		vsUpdated += 1

		// If this is an Ingress resource, add an event if there was a backend error
		if !correctBackend {
			if strings.HasSuffix(rsCfg.Virtual.VirtualServerName, "ingress") {
				appMgr.recordIngressEvent(nil, reason, msg,
					rsCfg.Virtual.VirtualServerName)
			}
		}
	}

	return true, vsFound, vsUpdated
}

func (appMgr *Manager) updatePoolMembersForNodePort(
	svc *v1.Service,
	svcKey serviceKey,
	rsCfg *ResourceConfig,
	index int,
) (bool, string, string) {
	if svc.Spec.Type == v1.ServiceTypeNodePort {
		for _, portSpec := range svc.Spec.Ports {
			if portSpec.Port == svcKey.ServicePort {
				log.Debugf("Service backend matched %+v: using node port %v",
					svcKey, portSpec.NodePort)
				rsCfg.MetaData.Active = true
				rsCfg.MetaData.NodePort = portSpec.NodePort
				rsCfg.Pools[index].Members =
					appMgr.getEndpointsForNodePort(portSpec.NodePort)
			}
		}
		return true, "", ""
	} else {
		msg := fmt.Sprintf("Requested service backend '%+v' not of NodePort type",
			svcKey.ServiceName)
		log.Debug(msg)
		return false, "IncorrectBackendServiceType", msg
	}
}

func (appMgr *Manager) updatePoolMembersForCluster(
	svc *v1.Service,
	sKey serviceKey,
	rsCfg *ResourceConfig,
	appInf *appInformer,
	index int,
) (bool, string, string) {
	svcKey := sKey.Namespace + "/" + sKey.ServiceName
	item, found, _ := appInf.endptInformer.GetStore().GetByKey(svcKey)
	if !found {
		msg := fmt.Sprintf("Endpoints for service '%v' not found!", svcKey)
		log.Debug(msg)
		return false, "EndpointsNotFound", msg
	}
	eps, _ := item.(*v1.Endpoints)
	for _, portSpec := range svc.Spec.Ports {
		if portSpec.Port == sKey.ServicePort {
			ipPorts := getEndpointsForService(portSpec.Name, eps)
			log.Debugf("Found endpoints for backend %+v: %v", sKey, ipPorts)
			rsCfg.MetaData.Active = true
			rsCfg.Pools[index].Members = ipPorts
		}
	}
	return true, "", ""
}

func (appMgr *Manager) deactivateVirtualServer(
	sKey serviceKey,
	rsName string,
	rsCfg *ResourceConfig,
	index int,
) bool {
	updateConfig := false
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	if rs, ok := appMgr.resources.Get(sKey, rsName); ok {
		rsCfg.MetaData.Active = false
		rsCfg.Pools[index].Members = nil
		if !reflect.DeepEqual(rs, rsCfg) {
			log.Debugf("Service delete matching backend %v %v deactivating config",
				sKey, rsName)
			updateConfig = true
		}
	} else {
		// We have a config map but not a server. Put in the virtual server from
		// the config map.
		updateConfig = true
	}
	if updateConfig {
		appMgr.resources.Assign(sKey, rsName, rsCfg)
	}
	return updateConfig
}

func (appMgr *Manager) saveVirtualServer(
	sKey serviceKey,
	rsName string,
	newRsCfg *ResourceConfig,
) bool {
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	if oldRsCfg, ok := appMgr.resources.Get(sKey, rsName); ok {
		if reflect.DeepEqual(oldRsCfg, newRsCfg) {
			// not changed, don't trigger a config write
			return false
		}
		log.Warningf("Overwriting existing entry for backend %+v", sKey)
	}
	appMgr.resources.Assign(sKey, rsName, newRsCfg)
	return true
}

func (appMgr *Manager) getResourcesForKey(sKey serviceQueueKey) ResourceMap {
	// Return a copy of what is stored in resources
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	rsMap := make(ResourceMap)
	appMgr.resources.ForEach(func(key serviceKey, cfg *ResourceConfig) {
		if key.Namespace == sKey.Namespace &&
			key.ServiceName == sKey.ServiceName {
			rsMap[key.ServicePort] =
				append(rsMap[key.ServicePort], cfg)
		}
	})
	return rsMap
}

func (appMgr *Manager) processAllMultiSvc(numPools int, rsName string) bool {
	// If multi-service and we haven't yet configured keys/cfgs for each service,
	// then we don't want to update
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	_, keys := appMgr.resources.GetAllWithName(rsName)
	if len(keys) != numPools {
		return false
	}
	return true
}

func (appMgr *Manager) deleteUnusedResources(
	sKey serviceQueueKey,
	rsMap ResourceMap) int {
	rsDeleted := 0
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	for port, cfgList := range rsMap {
		tmpKey := serviceKey{
			Namespace:   sKey.Namespace,
			ServiceName: sKey.ServiceName,
			ServicePort: port,
		}
		for _, cfg := range cfgList {
			rsName := cfg.Virtual.VirtualServerName
			if appMgr.resources.Delete(tmpKey, rsName) {
				rsDeleted += 1
			}
		}
	}
	return rsDeleted
}

// If a route is deleted, loop through other route configs and delete pools/rules/profiles
// for the deleted route.
func (appMgr *Manager) deleteUnusedRoutes(namespace string) {
	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	var routeName string
	appMgr.resources.ForEach(func(key serviceKey, cfg *ResourceConfig) {
		if cfg.MetaData.ResourceType == "route" {
			for i, pool := range cfg.Pools {
				sKey := serviceKey{
					ServiceName: pool.ServiceName,
					ServicePort: pool.ServicePort,
					Namespace:   key.Namespace,
				}
				if _, ok := appMgr.resources.Get(sKey, cfg.Virtual.VirtualServerName); !ok {
					poolName := fmt.Sprintf("/%s/%s", cfg.Virtual.Partition, pool.Name)
					// Delete rule
					for _, pol := range cfg.Policies {
						if len(pol.Rules) == 1 {
							nr := nameRef{
								Name:      pol.Name,
								Partition: pol.Partition,
							}
							cfg.RemovePolicy(nr)
							continue
						}
						for i, rule := range pol.Rules {
							if len(rule.Actions) > 0 && rule.Actions[0].Pool == poolName {
								routeName = strings.Split(rule.Name, "_")[3]
								if i >= len(pol.Rules)-1 {
									pol.Rules = pol.Rules[:len(pol.Rules)-1]
								} else {
									copy(pol.Rules[i:], pol.Rules[i+1:])
									pol.Rules[len(pol.Rules)-1] = &Rule{}
									pol.Rules = pol.Rules[:len(pol.Rules)-1]
								}
								cfg.SetPolicy(pol)
							}
						}
					}
					// Delete pool
					if i >= len(cfg.Pools)-1 {
						cfg.Pools = cfg.Pools[:len(cfg.Pools)-1]
					} else {
						copy(cfg.Pools[i:], cfg.Pools[i+1:])
						cfg.Pools[len(cfg.Pools)-1] = Pool{}
						cfg.Pools = cfg.Pools[:len(cfg.Pools)-1]
					}
					// Delete profile
					if routeName != "" {
						profileName := fmt.Sprintf("%s/%s-https-cert",
							cfg.Virtual.Partition, routeName)
						cfg.Virtual.RemoveFrontendSslProfileName(profileName)
					}
				}
			}
			appMgr.resources.Assign(key, cfg.Virtual.VirtualServerName, cfg)
		}
	})
}

func (appMgr *Manager) deleteUnusedProfiles(namespace string) {
	var found bool
	appMgr.customProfiles.Lock()
	defer appMgr.customProfiles.Unlock()
	for key, profile := range appMgr.customProfiles.profs {
		found = false
		appMgr.resources.ForEach(func(k serviceKey, rsCfg *ResourceConfig) {
			if key.ResourceName == rsCfg.Virtual.VirtualServerName &&
				key.Namespace == namespace {
				if rsCfg.Virtual.ReferencesProfile(profile) {
					found = true
				}
			}
		})
		if !found {
			delete(appMgr.customProfiles.profs, key)
		}
	}
}

func (appMgr *Manager) setBindAddrAnnotation(
	cm *v1.ConfigMap,
	sKey serviceQueueKey,
	rsCfg *ResourceConfig,
) {
	var doUpdate bool
	if cm.ObjectMeta.Annotations == nil {
		cm.ObjectMeta.Annotations = make(map[string]string)
		doUpdate = true
	} else if cm.ObjectMeta.Annotations[vsBindAddrAnnotation] !=
		rsCfg.Virtual.VirtualAddress.BindAddr {
		doUpdate = true
	}
	if doUpdate {
		cm.ObjectMeta.Annotations[vsBindAddrAnnotation] =
			rsCfg.Virtual.VirtualAddress.BindAddr
		_, err := appMgr.kubeClient.CoreV1().ConfigMaps(sKey.Namespace).Update(cm)
		if nil != err {
			log.Warningf("Error when creating status IP annotation: %s", err)
		} else {
			log.Debugf("Updating ConfigMap %+v annotation - %v: %v",
				sKey, vsBindAddrAnnotation,
				rsCfg.Virtual.VirtualAddress.BindAddr)
		}
	}
}

func (appMgr *Manager) setIngressStatus(
	ing *v1beta1.Ingress,
	rsCfg *ResourceConfig,
) {
	// Set the ingress status to include the virtual IP
	lbIngress := v1.LoadBalancerIngress{IP: rsCfg.Virtual.VirtualAddress.BindAddr}
	if len(ing.Status.LoadBalancer.Ingress) == 0 {
		ing.Status.LoadBalancer.Ingress = append(ing.Status.LoadBalancer.Ingress, lbIngress)
	} else if ing.Status.LoadBalancer.Ingress[0].IP != rsCfg.Virtual.VirtualAddress.BindAddr {
		ing.Status.LoadBalancer.Ingress[0] = lbIngress
	}
	_, updateErr := appMgr.kubeClient.ExtensionsV1beta1().
		Ingresses(ing.ObjectMeta.Namespace).UpdateStatus(ing)
	if nil != updateErr {
		// Multi-service causes the controller to try to update the status multiple times
		// at once. Ignore this error.
		if strings.Contains(updateErr.Error(), "object has been modified") {
			return
		}
		warning := fmt.Sprintf(
			"Error when setting Ingress status IP for virtual server %v: %v",
			rsCfg.Virtual.VirtualServerName, updateErr)
		log.Warning(warning)
		appMgr.recordIngressEvent(ing, "StatusIPError", warning, "")
	}
}

// This function expects either an Ingress resource or the name of a VS for an Ingress
func (appMgr *Manager) recordIngressEvent(ing *v1beta1.Ingress,
	reason,
	message,
	rsName string) {
	var namespace string
	var name string
	if ing != nil {
		namespace = ing.ObjectMeta.Namespace
	} else {
		namespace = strings.Split(rsName, "_")[0]
		name = rsName[len(namespace)+1 : len(rsName)-len("-ingress")]
	}
	appMgr.broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{
		Interface: appMgr.kubeClient.Core().Events(namespace)})

	// If we aren't given an Ingress resource, we use the name to find it
	var err error
	if ing == nil {
		ing, err = appMgr.kubeClient.Extensions().Ingresses(namespace).
			Get(name, metav1.GetOptions{})
		if nil != err {
			log.Warningf("Could not find Ingress resource '%v'.", name)
			return
		}
	}

	// Create the event
	appMgr.eventRecorder.Event(ing, v1.EventTypeNormal, reason, message)
}

func getEndpointsForService(
	portName string,
	eps *v1.Endpoints,
) []Member {
	var members []Member

	if eps == nil {
		return members
	}

	for _, subset := range eps.Subsets {
		for _, p := range subset.Ports {
			if portName == p.Name {
				for _, addr := range subset.Addresses {
					member := Member{
						Address: addr.IP,
						Port:    p.Port,
						Session: "user-enabled",
					}
					members = append(members, member)
				}
			}
		}
	}
	return members
}

func (appMgr *Manager) getEndpointsForNodePort(
	nodePort int32,
) []Member {
	nodes := appMgr.getNodesFromCache()
	var members []Member
	for _, v := range nodes {
		member := Member{
			Address: v,
			Port:    nodePort,
			Session: "user-enabled",
		}
		members = append(members, member)
	}

	return members
}

func handleConfigMapParseFailure(
	appMgr *Manager,
	cm *v1.ConfigMap,
	cfg *ResourceConfig,
	err error,
) bool {
	log.Warningf("Could not get config for ConfigMap: %v - %v",
		cm.ObjectMeta.Name, err)
	// If virtual server exists for invalid configmap, delete it
	var serviceName string
	var servicePort int32
	if nil != cfg {
		if len(cfg.Pools) == 0 {
			serviceName = ""
			servicePort = 0
		} else {
			serviceName = cfg.Pools[0].ServiceName
			servicePort = cfg.Pools[0].ServicePort
		}
		sKey := serviceKey{serviceName, servicePort, cm.ObjectMeta.Namespace}
		rsName := formatConfigMapVSName(cm)
		if _, ok := appMgr.resources.Get(sKey, rsName); ok {
			appMgr.resources.Lock()
			defer appMgr.resources.Unlock()
			appMgr.resources.Delete(sKey, rsName)
			delete(cm.ObjectMeta.Annotations, vsBindAddrAnnotation)
			appMgr.kubeClient.CoreV1().ConfigMaps(cm.ObjectMeta.Namespace).Update(cm)
			log.Warningf("Deleted virtual server associated with ConfigMap: %v",
				cm.ObjectMeta.Name)
			return true
		}
	}
	return false
}

// Check for a change in Node state
func (appMgr *Manager) ProcessNodeUpdate(
	obj interface{}, err error,
) {
	if nil != err {
		log.Warningf("Unable to get list of nodes, err=%+v", err)
		return
	}

	newNodes, err := appMgr.getNodeAddresses(obj)
	if nil != err {
		log.Warningf("Unable to get list of nodes, err=%+v", err)
		return
	}
	sort.Strings(newNodes)

	appMgr.resources.Lock()
	defer appMgr.resources.Unlock()
	appMgr.oldNodesMutex.Lock()
	defer appMgr.oldNodesMutex.Unlock()

	// Only check for updates once we are in our initial state
	if appMgr.initialState {
		// Compare last set of nodes with new one
		if !reflect.DeepEqual(newNodes, appMgr.oldNodes) {
			log.Infof("ProcessNodeUpdate: Change in Node state detected")
			appMgr.resources.ForEach(func(key serviceKey, cfg *ResourceConfig) {
				var members []Member
				for _, node := range newNodes {
					member := Member{
						Address: node,
						Port:    cfg.MetaData.NodePort,
						Session: "user-enabled",
					}
					members = append(members, member)
				}
				cfg.Pools[0].Members = members
			})
			// Output the Big-IP config
			appMgr.outputConfigLocked()

			// Update node cache
			appMgr.oldNodes = newNodes
		}
	} else {
		// Initialize appMgr nodes on our first pass through
		appMgr.oldNodes = newNodes
	}
}

// Return a copy of the node cache
func (appMgr *Manager) getNodesFromCache() []string {
	appMgr.oldNodesMutex.Lock()
	defer appMgr.oldNodesMutex.Unlock()
	nodes := make([]string, len(appMgr.oldNodes))
	copy(nodes, appMgr.oldNodes)

	return nodes
}

// Get a list of Node addresses
func (appMgr *Manager) getNodeAddresses(
	obj interface{},
) ([]string, error) {
	nodes, ok := obj.([]v1.Node)
	if false == ok {
		return nil,
			fmt.Errorf("poll update unexpected type, interface is not []v1.Node")
	}

	addrs := []string{}

	var addrType v1.NodeAddressType
	if appMgr.UseNodeInternal() {
		addrType = v1.NodeInternalIP
	} else {
		addrType = v1.NodeExternalIP
	}

	for _, node := range nodes {
		if node.Spec.Unschedulable {
			// Skip master node
			continue
		} else {
			nodeAddrs := node.Status.Addresses
			for _, addr := range nodeAddrs {
				if addr.Type == addrType {
					addrs = append(addrs, addr.Address)
				}
			}
		}
	}

	return addrs, nil
}
