package operator

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	kubeset "k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	operatorapi "github.com/openshift/api/operator/v1alpha1"
	routeset "github.com/openshift/client-go/route/clientset/versioned"
	routeinformers "github.com/openshift/client-go/route/informers/externalversions"

	regopapi "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1alpha1"
	regopclient "github.com/openshift/cluster-image-registry-operator/pkg/client"
	"github.com/openshift/cluster-image-registry-operator/pkg/clusteroperator"
	regopset "github.com/openshift/cluster-image-registry-operator/pkg/generated/clientset/versioned"
	regopinformers "github.com/openshift/cluster-image-registry-operator/pkg/generated/informers/externalversions"
	regoplisters "github.com/openshift/cluster-image-registry-operator/pkg/generated/listers/imageregistry/v1alpha1"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource"
)

const (
	workqueueKey          = "changes"
	defaultResyncDuration = 10 * time.Minute
)

type permanentError struct {
	Err error
}

func (e permanentError) Error() string {
	return e.Err.Error()
}

func NewController(kubeconfig *restclient.Config, namespace string) (*Controller, error) {
	operatorNamespace, err := regopclient.GetWatchNamespace()
	if err != nil {
		glog.Fatalf("Failed to get watch namespace: %v", err)
	}

	operatorName, err := regopclient.GetOperatorName()
	if err != nil {
		glog.Fatalf("Failed to get operator name: %v", err)
	}

	p := parameters.Globals{}

	p.Deployment.Namespace = namespace
	p.Deployment.Labels = map[string]string{"docker-registry": "default"}

	p.Pod.ServiceAccount = "registry"
	p.Container.Port = 5000

	p.Healthz.Route = "/healthz"
	p.Healthz.TimeoutSeconds = 5

	p.Service.Name = "image-registry"
	p.ImageConfig.Name = "cluster"

	c := &Controller{
		kubeconfig:    kubeconfig,
		params:        p,
		generator:     resource.NewGenerator(kubeconfig, &p),
		clusterStatus: clusteroperator.NewStatusHandler(kubeconfig, operatorName, operatorNamespace),
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Changes"),
	}

	if err = c.Bootstrap(); err != nil {
		return nil, err
	}

	return c, nil
}

type Listers struct {
	Deployments   appslisters.DeploymentNamespaceLister
	Services      corelisters.ServiceNamespaceLister
	ImageRegistry regoplisters.ImageRegistryLister
}

type Controller struct {
	kubeconfig    *restclient.Config
	params        parameters.Globals
	generator     *resource.Generator
	clusterStatus *clusteroperator.StatusHandler
	workqueue     workqueue.RateLimitingInterface
	listers       Listers
}

func (c *Controller) createOrUpdateResources(cr *regopapi.ImageRegistry, modified *bool) error {
	appendFinalizer(cr, modified)

	err := verifyResource(cr, &c.params)
	if err != nil {
		return permanentError{Err: fmt.Errorf("unable to complete resource: %s", err)}
	}

	err = c.generator.Apply(cr, modified)
	if err != nil {
		return err
	}

	return nil
}

func (c *Controller) CreateOrUpdateResources(cr *regopapi.ImageRegistry, modified *bool) error {
	if cr.Spec.ManagementState != operatorapi.Managed {
		return nil
	}

	return c.createOrUpdateResources(cr, modified)
}

func (c *Controller) sync() error {
	client, err := regopset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	cr, err := c.listers.ImageRegistry.Get(resourceName(c.params.Deployment.Namespace))
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Bootstrap()
		}
		return fmt.Errorf("failed to get %q custom resource: %s", cr.Name, err)
	}

	if cr.ObjectMeta.DeletionTimestamp != nil {
		return c.finalizeResources(cr)
	}

	var statusChanged bool
	var applyError error
	removed := false
	switch cr.Spec.ManagementState {
	case operatorapi.Removed:
		applyError = c.RemoveResources(cr)
		removed = true
	case operatorapi.Managed:
		applyError = c.CreateOrUpdateResources(cr, &statusChanged)
		if applyError == nil {
			svc, err := c.listers.Services.Get(c.params.Service.Name)
			if err == nil {
				svcHostname := fmt.Sprintf("%s.%s.svc:%d", svc.Name, svc.Namespace, svc.Spec.Ports[0].Port)
				if cr.Status.InternalRegistryHostname != svcHostname {
					cr.Status.InternalRegistryHostname = svcHostname
					statusChanged = true
				}
			} else if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get %q service %s", c.params.Service.Name, err)
			}
		}
	case operatorapi.Unmanaged:
		// ignore
	default:
		glog.Warningf("unknown custom resource state: %s", cr.Spec.ManagementState)
	}

	deploy, err := c.listers.Deployments.Get(cr.ObjectMeta.Name)
	if errors.IsNotFound(err) {
		deploy = nil
	} else if err != nil {
		return fmt.Errorf("failed to get %q deployment: %s", cr.ObjectMeta.Name, err)
	}

	c.syncStatus(cr, deploy, applyError, removed, &statusChanged)

	if statusChanged {
		glog.Infof("status changed: %s", objectInfo(cr))

		cr.Status.ObservedGeneration = cr.Generation

		_, err = client.Imageregistry().ImageRegistries().Update(cr)
		if err != nil {
			if !errors.IsConflict(err) {
				glog.Errorf("unable to update %s: %s", objectInfo(cr), err)
			}
			return err
		}
	}

	if _, ok := applyError.(permanentError); !ok {
		return applyError
	}

	return nil
}

func (c *Controller) eventProcessor() {
	for {
		obj, shutdown := c.workqueue.Get()

		if shutdown {
			return
		}

		err := func(obj interface{}) error {
			defer c.workqueue.Done(obj)

			if _, ok := obj.(string); !ok {
				c.workqueue.Forget(obj)
				glog.Errorf("expected string in workqueue but got %#v", obj)
				return nil
			}

			if err := c.sync(); err != nil {
				c.workqueue.AddRateLimited(workqueueKey)
				return fmt.Errorf("unable to sync: %s, requeuing", err)
			}

			c.workqueue.Forget(obj)

			glog.Infof("event from workqueue successfully processed")
			return nil
		}(obj)

		if err != nil {
			glog.Errorf("unable to process event: %s", err)
		}
	}
}

func (c *Controller) handler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			glog.V(1).Infof("add event to workqueue due to %s (add)", objectInfo(o))
			c.workqueue.AddRateLimited(workqueueKey)
		},
		UpdateFunc: func(o, n interface{}) {
			newAccessor, err := kmeta.Accessor(n)
			if err != nil {
				glog.Errorf("unable to get accessor for new object: %s", err)
				return
			}
			oldAccessor, err := kmeta.Accessor(o)
			if err != nil {
				glog.Errorf("unable to get accessor for old object: %s", err)
				return
			}
			if newAccessor.GetResourceVersion() == oldAccessor.GetResourceVersion() {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}
			glog.V(1).Infof("add event to workqueue due to %s (update)", objectInfo(n))
			c.workqueue.AddRateLimited(workqueueKey)
		},
		DeleteFunc: func(o interface{}) {
			object, ok := o.(metaapi.Object)
			if !ok {
				tombstone, ok := o.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("error decoding object, invalid type")
					return
				}
				object, ok = tombstone.Obj.(metaapi.Object)
				if !ok {
					glog.Errorf("error decoding object tombstone, invalid type")
					return
				}
				glog.V(4).Infof("recovered deleted object %q from tombstone", object.GetName())
			}
			glog.V(1).Infof("add event to workqueue due to %s (delete)", objectInfo(object))
			c.workqueue.AddRateLimited(workqueueKey)
		},
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.workqueue.ShutDown()

	err := c.clusterStatus.Create()
	if err != nil {
		glog.Errorf("unable to create cluster operator resource: %s", err)
	}

	kubeClient, err := kubeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	routeClient, err := routeset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	regopClient, err := regopset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResyncDuration, kubeinformers.WithNamespace(c.params.Deployment.Namespace))
	routeInformerFactory := routeinformers.NewSharedInformerFactoryWithOptions(routeClient, defaultResyncDuration, routeinformers.WithNamespace(c.params.Deployment.Namespace))
	regopInformerFactory := regopinformers.NewSharedInformerFactory(regopClient, defaultResyncDuration)

	var informers []cache.SharedIndexInformer
	for _, ctor := range []func() cache.SharedIndexInformer{
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Apps().V1().Deployments()
			c.listers.Deployments = informer.Lister().Deployments(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Services()
			c.listers.Services = informer.Lister().Services(c.params.Deployment.Namespace)
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().Secrets()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ConfigMaps()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Core().V1().ServiceAccounts()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoles()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := kubeInformerFactory.Rbac().V1().ClusterRoleBindings()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := routeInformerFactory.Route().V1().Routes()
			return informer.Informer()
		},
		func() cache.SharedIndexInformer {
			informer := regopInformerFactory.Imageregistry().V1alpha1().ImageRegistries()
			c.listers.ImageRegistry = informer.Lister()
			return informer.Informer()
		},
	} {
		informer := ctor()
		informer.AddEventHandler(c.handler())
		informers = append(informers, informer)
	}

	kubeInformerFactory.Start(stopCh)
	routeInformerFactory.Start(stopCh)
	regopInformerFactory.Start(stopCh)

	glog.Info("waiting for informer caches to sync")
	for _, informer := range informers {
		if ok := cache.WaitForCacheSync(stopCh, informer.HasSynced); !ok {
			return fmt.Errorf("failed to wait for caches to sync")
		}
	}

	go wait.Until(c.eventProcessor, time.Second, stopCh)

	glog.Info("started events processor")
	<-stopCh
	glog.Info("shutting down events processor")

	return nil
}
