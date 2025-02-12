package staging

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	s2hv1 "github.com/agoda-com/samsahai/api/v1"
	"github.com/agoda-com/samsahai/internal"
	s2herrors "github.com/agoda-com/samsahai/internal/errors"
	s2hlog "github.com/agoda-com/samsahai/internal/log"
	"github.com/agoda-com/samsahai/internal/staging/deploy/helm3"
	"github.com/agoda-com/samsahai/internal/staging/deploy/mock"
	"github.com/agoda-com/samsahai/internal/staging/testrunner/gitlab"
	"github.com/agoda-com/samsahai/internal/staging/testrunner/teamcity"
	"github.com/agoda-com/samsahai/internal/staging/testrunner/testmock"
	samsahairpc "github.com/agoda-com/samsahai/pkg/samsahai/rpc"
	stagingrpc "github.com/agoda-com/samsahai/pkg/staging/rpc"
)

var logger = s2hlog.Log.WithName(internal.StagingCtrlName)

const DefaultCleanupTimeout = 15 * time.Minute

type controller struct {
	deployEngines map[string]internal.DeployEngine
	testRunners   map[string]internal.StagingTestRunner

	teamName   string
	namespace  string
	authToken  string
	queueCtrl  internal.QueueController
	configCtrl internal.ConfigController
	client     client.Client
	scheme     *apiruntime.Scheme

	internalStop    <-chan struct{}
	internalStopper chan<- struct{}
	rpcHandler      stagingrpc.TwirpServer

	currentQueue *s2hv1.Queue
	mtQueue      sync.Mutex
	s2hClient    samsahairpc.RPC

	lastAppliedValues       map[string]interface{}
	lastStableComponentList s2hv1.StableComponentList

	teamcityBaseURL  string
	teamcityUsername string
	teamcityPassword string

	gitlabBaseURL string
	gitlabToken   string

	configs internal.StagingConfig
}

// TODO: move test runner config to be optional
func NewController(
	teamName string,
	namespace string,
	authToken string,
	s2hClient samsahairpc.RPC,
	mgr manager.Manager,
	queueCtrl internal.QueueController,
	configCtrl internal.ConfigController,
	teamcityBaseURL string,
	teamcityUsername string,
	teamcityPassword string,
	gitlabBaseURL string,
	gitlabToken string,
	configs internal.StagingConfig,
) internal.StagingController {
	if queueCtrl == nil {
		logger.Error(s2herrors.ErrInternalError, "queue ctrl cannot be nil")
		panic(s2herrors.ErrInternalError)
	}

	stopper := make(chan struct{})
	c := &controller{
		deployEngines:           map[string]internal.DeployEngine{},
		testRunners:             map[string]internal.StagingTestRunner{},
		teamName:                teamName,
		namespace:               namespace,
		authToken:               authToken,
		s2hClient:               s2hClient,
		queueCtrl:               queueCtrl,
		configCtrl:              configCtrl,
		client:                  mgr.GetClient(),
		scheme:                  mgr.GetScheme(),
		internalStop:            stopper,
		internalStopper:         stopper,
		lastAppliedValues:       nil,
		lastStableComponentList: s2hv1.StableComponentList{},
		teamcityBaseURL:         teamcityBaseURL,
		teamcityUsername:        teamcityUsername,
		teamcityPassword:        teamcityPassword,
		gitlabBaseURL:           gitlabBaseURL,
		gitlabToken:             gitlabToken,
		configs:                 configs,
	}

	c.rpcHandler = stagingrpc.NewRPCServer(c, nil)

	c.loadDeployEngines()
	c.loadTestRunners()

	return c
}

func (c *controller) Start(stop <-chan struct{}) {
	defer close(c.internalStopper)

	concurrentProcess := 1
	jitterPeriod := time.Millisecond * 1000
	for i := 0; i < concurrentProcess; i++ {
		go wait.Until(func() {
			for c.process() {
			}
		}, jitterPeriod, c.internalStop)
	}

	logger.Debug(fmt.Sprintf("%s is running", internal.StagingCtrlName))

	<-stop

	logger.Info(fmt.Sprintf("%s is shutting down", internal.StagingCtrlName))
}

func (c *controller) process() bool {
	var err error
	if c.getCurrentQueue() == nil {
		c.mtQueue.Lock()
		// pick new queue
		obj, err := c.queueCtrl.First(c.namespace)
		if err != nil {
			logger.Error(err, "cannot pick the first component of queue")
			c.mtQueue.Unlock()
			return false
		}

		if obj != nil {
			var ok bool
			c.currentQueue, ok = obj.(*s2hv1.Queue)
			if !ok {
				logger.Error(err, "cannot parse runtime object into queue object")
				c.mtQueue.Unlock()
				return false
			}
		}

		c.mtQueue.Unlock()
	}

	// no queue
	if c.getCurrentQueue() == nil {
		time.Sleep(2 * time.Second)
		return true
	}

	// try to get current queue from k8s
	// if queue is not deleting or cancelling
	if c.isQueueStateValid() {
		if err := c.syncQueueWithK8s(); err != nil {
			return false
		}
	}

	queue := c.getCurrentQueue()

	switch queue.Spec.Type {
	case s2hv1.QueueTypePromoteToActive, s2hv1.QueueTypeDemoteFromActive:
		switch queue.Status.State {
		case "", s2hv1.Waiting:
			queue.Status.NoOfProcessed++
			err = c.updateQueueWithState(queue, s2hv1.DetectingImageMissing)
		case s2hv1.DetectingImageMissing:
			err = c.detectImageMissing(queue)
		case s2hv1.Creating:
			err = c.deployEnvironment(queue)
		case s2hv1.Testing:
			err = c.updateQueueWithState(queue, s2hv1.Collecting)
		case s2hv1.Collecting:
			err = c.collectResult(queue)
		case s2hv1.Cancelling:
			err = c.cancelQueue(queue)
		case s2hv1.Finished:
		}
	default:
		switch queue.Status.State {
		case "", s2hv1.Waiting:
			err = c.initQueue(queue)
		case s2hv1.CleaningBefore:
			err = c.cleanBefore(queue)
		case s2hv1.DetectingImageMissing:
			err = c.detectImageMissing(queue)
		case s2hv1.Creating:
			err = c.deployEnvironment(queue)
		case s2hv1.Testing:
			err = c.startTesting(queue)
		case s2hv1.Collecting:
			err = c.collectResult(queue)
		case s2hv1.CleaningAfter:
			err = c.cleanAfter(queue)
		case s2hv1.Deleting:
			err = c.deleteQueue(queue)
		case s2hv1.Cancelling:
			err = c.cancelQueue(queue)
		case s2hv1.Finished:
		default:
		}
	}

	return err != nil
}

func (c *controller) loadDeployEngines() {
	// init test runner
	engines := []internal.DeployEngine{
		mock.New(),
		helm3.New(c.namespace, true),
	}

	for _, e := range engines {
		if e == nil {
			continue
		}

		c.deployEngines[e.GetName()] = e
	}
}

func (c *controller) loadTestRunners() {
	// init test runner
	testRunners := []internal.StagingTestRunner{
		testmock.New(),
	}

	// TODO: should load teamcity credentials from secret, default from samsahai
	if c.teamcityBaseURL != "" && c.teamcityUsername != "" && c.teamcityPassword != "" {
		testRunners = append(testRunners, teamcity.New(c.client, c.teamcityBaseURL, c.teamcityUsername, c.teamcityPassword))
	}

	if c.gitlabBaseURL != "" {
		testRunners = append(testRunners, gitlab.New(c.client, c.gitlabBaseURL, gitlab.WithGitlabToken(c.gitlabToken)))
	}

	for _, r := range testRunners {
		if r == nil {
			continue
		}

		c.testRunners[r.GetName()] = r
	}
}

func (c *controller) IsBusy() bool {
	return c.getCurrentQueue() != nil
}

func (c *controller) LoadTestRunner(runner internal.StagingTestRunner) {
	if runner == nil || runner.GetName() == "" {
		return
	}
	c.testRunners[runner.GetName()] = runner
}

func (c *controller) LoadDeployEngine(engine internal.DeployEngine) {
	if engine == nil || engine.GetName() == "" {
		return
	}
	c.deployEngines[engine.GetName()] = engine
}

// isQueueValid returns true if Queue not in Deleting and Cancelling state
func (c *controller) isQueueStateValid() bool {
	q := c.getCurrentQueue()
	return q.Status.State != s2hv1.Deleting && q.Status.State != s2hv1.Cancelling
}

// syncQueueWithK8s fetches Queue from k8s and set it to currentQueue if mismatch
func (c *controller) syncQueueWithK8s() error {
	var err error

	q := c.getCurrentQueue()
	fetched := &s2hv1.Queue{}
	err = c.client.Get(context.TODO(), types.NamespacedName{
		Namespace: q.GetNamespace(),
		Name:      q.GetName()}, fetched)
	if err != nil && k8serrors.IsNotFound(err) {
		// queue not found
		// delete by user
		logger.Debug(fmt.Sprintf("queue: %s/%s got cancel", q.GetNamespace(), q.GetName()))
		c.mtQueue.Lock()
		c.currentQueue.SetState(s2hv1.Cancelling)
		c.mtQueue.Unlock()
	} else if err != nil {
		logger.Error(err, fmt.Sprintf("cannot get queue: %s/%s", q.GetNamespace(), q.GetName()))
		return err
	} else if !reflect.DeepEqual(fetched, q) {
		// update current queue
		c.mtQueue.Lock()
		c.currentQueue = fetched
		c.mtQueue.Unlock()
	}

	return nil
}

func (c *controller) initQueue(q *s2hv1.Queue) error {
	deployConfig := c.getDeployConfiguration(q)
	if deployConfig == nil {
		err := fmt.Errorf("cannot get deployment configuration, namespace: %s, queue: %s", c.namespace, q.Name)
		logger.Error(err, "cannot init queue", "queue", q.Name, "namespace", c.namespace)
		return err
	}

	q.Status.NoOfProcessed++
	q.Status.QueueHistoryName = generateQueueHistoryName(q.Name)
	if deployConfig.Engine != nil {
		if _, ok := c.deployEngines[*deployConfig.Engine]; ok {
			q.Status.DeployEngine = *deployConfig.Engine
		}
	}
	q.Status.SetCondition(s2hv1.QueueCleaningBeforeStarted, corev1.ConditionTrue,
		"starts cleaning the namespace before running task")

	return c.updateQueueWithState(q, s2hv1.CleaningBefore)
}

func (c *controller) cleanBefore(queue *s2hv1.Queue) error {
	deployEngine := c.getDeployEngine(queue)
	parentComps, err := c.configCtrl.GetParentComponents(c.teamName)
	if err != nil {
		return err
	}

	if !queue.Status.IsConditionTrue(s2hv1.QueueCleanedBefore) {
		for compName := range parentComps {
			refName := internal.GenReleaseName(c.namespace, compName)
			if err := deployEngine.Delete(refName); err != nil {
				logger.Error(err, "cannot delete release",
					"refName", refName,
					"namespace", c.namespace,
					"component", compName)
			}
		}
	}

	cleanupTimeout := time.Duration(0)
	deployConfig := c.getDeployConfiguration(queue)
	if deployConfig != nil {
		cleanupTimeout = deployConfig.ComponentCleanupTimeout.Duration
	}

	isCleaned, err := WaitForComponentsCleaned(
		c.client,
		deployEngine,
		parentComps,
		c.namespace,
		queue.Status.GetConditionLatestTime(s2hv1.QueueCleaningBeforeStarted),
		cleanupTimeout)
	if err != nil {
		return err
	} else if !isCleaned {
		logger.Warn("waiting for component cleaned",
			"queue", queue.Name, "state", s2hv1.CleaningBefore)
		time.Sleep(2 * time.Second)
		return nil
	}

	queue.Status.SetCondition(
		s2hv1.QueueCleanedBefore,
		corev1.ConditionTrue,
		"namespace cleaned")

	return c.updateQueueWithState(queue, s2hv1.DetectingImageMissing)
}

func (c *controller) cleanAfter(queue *s2hv1.Queue) error {
	deployEngine := c.getDeployEngine(queue)

	parentComps, err := c.configCtrl.GetParentComponents(c.teamName)
	if err != nil {
		return err
	}

	if !queue.Status.IsConditionTrue(s2hv1.QueueCleanedAfter) {
		for compName := range parentComps {
			refName := internal.GenReleaseName(c.namespace, compName)
			if err := deployEngine.Delete(refName); err != nil {
				logger.Error(err, "cannot delete release",
					"refName", refName,
					"namespace", c.namespace,
					"component", compName)
			}
		}
	}

	cleanupTimeout := time.Duration(0)
	deployConfig := c.getDeployConfiguration(queue)
	if deployConfig != nil {
		cleanupTimeout = deployConfig.ComponentCleanupTimeout.Duration
	}

	isCleaned, err := WaitForComponentsCleaned(
		c.client,
		deployEngine,
		parentComps,
		c.namespace,
		queue.Status.GetConditionLatestTime(s2hv1.QueueCleaningAfterStarted),
		cleanupTimeout)
	if err != nil {
		return err
	} else if !isCleaned {
		logger.Warn("waiting for component cleaned",
			"queue", queue.Name, "state", s2hv1.CleaningAfter)
		time.Sleep(2 * time.Second)
		return nil
	}

	queue.Status.SetCondition(s2hv1.QueueCleanedAfter, corev1.ConditionTrue, "namespace cleaned")

	return c.updateQueueWithState(queue, s2hv1.Deleting)
}

func (c *controller) cancelQueue(q *s2hv1.Queue) error {
	c.clearCurrentQueue()
	return nil
}

func (c *controller) getConfiguration() (*s2hv1.ConfigSpec, error) {
	config, err := c.getConfigController().Get(c.teamName)
	if err != nil {
		return &s2hv1.ConfigSpec{}, err
	}

	return &config.Status.Used, nil
}

func (c *controller) getConfigController() internal.ConfigController {
	return c.configCtrl
}

func WaitForComponentsCleaned(
	c client.Client,
	deployEngine internal.DeployEngine,
	parentComps map[string]*s2hv1.Component,
	namespace string,
	startCleaningTime *metav1.Time,
	cleanupTimeout time.Duration,
) (bool, error) {
	if deployEngine.IsMocked() {
		return true, nil
	}

	forceClean := false
	if IsCleanupTimeout(startCleaningTime, cleanupTimeout) {
		forceClean = true
	}

	for compName := range parentComps {
		refName := internal.GenReleaseName(namespace, compName)
		selectors := deployEngine.GetLabelSelectors(refName)
		listOpt := &client.ListOptions{Namespace: namespace, LabelSelector: labels.SelectorFromSet(selectors)}
		log := logger.WithValues(
			"refName", refName,
			"namespace", namespace,
			"component", compName,
			"selectors", selectors)

		if forceClean {
			if err := deployEngine.ForceDelete(refName); err != nil {
				log.Error(err, "error while force delete")
			}
		}

		// check pods
		pods := &corev1.PodList{}
		if err := c.List(context.TODO(), pods, listOpt); err != nil {
			log.Error(err, "list pods error")
			return false, err
		}

		if len(pods.Items) > 0 {
			if forceClean {
				return false, forceCleanupPod(log, c, namespace, selectors)
			}

			return false, nil
		}

		// check services
		services := &corev1.ServiceList{}
		if err := c.List(context.TODO(), services, listOpt); err != nil {
			log.Error(err, "list services error")
			return false, err
		}

		if len(services.Items) > 0 {
			if forceClean {
				return false, forceCleanupService(log, c, services)
			}

			return false, nil
		}

		// check pvcs
		pvcs := &corev1.PersistentVolumeClaimList{}
		if err := c.List(context.TODO(), pvcs, listOpt); err != nil {
			log.Error(err, "list pvcs error")
			return false, err
		}

		if len(pvcs.Items) > 0 {
			log.Debug("pvc found, deleting")
			if err := c.DeleteAllOf(context.TODO(), &corev1.PersistentVolumeClaim{},
				client.InNamespace(namespace),
				client.MatchingLabels(selectors),
				client.PropagationPolicy(metav1.DeletePropagationBackground),
			); err != nil {
				log.Error(err, "delete all pvc error")
			}
			return false, nil
		}
	}

	return true, nil
}

func IsCleanupTimeout(start *metav1.Time, timeout time.Duration) bool {
	// if started time or timeout values are nil, no timeout
	if start == nil {
		return false
	}

	if timeout == 0 {
		timeout = DefaultCleanupTimeout
	}

	now := metav1.Now()
	return now.Sub(start.Time) > timeout
}

func forceCleanupPod(log s2hlog.Logger, c client.Client, namespace string, selectors map[string]string) error {
	ctx := context.Background()
	var err error

	log.Warn("force delete deployment")
	if err = c.DeleteAllOf(ctx,
		&appsv1.Deployment{},
		client.InNamespace(namespace),
		client.MatchingLabels(selectors),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		log.Error(err, "delete deployment error")
	}

	log.Warn("force delete statefulset")
	if err = c.DeleteAllOf(ctx,
		&appsv1.StatefulSet{},
		client.InNamespace(namespace),
		client.MatchingLabels(selectors),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		log.Error(err, "delete statefulset error")
	}

	log.Warn("force delete daemonset")
	if err = c.DeleteAllOf(ctx,
		&appsv1.DaemonSet{},
		client.InNamespace(namespace),
		client.MatchingLabels(selectors),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		log.Error(err, "delete daemonset error")
	}

	log.Warn("force delete job")
	if err = c.DeleteAllOf(ctx,
		&batchv1.Job{},
		client.InNamespace(namespace),
		client.MatchingLabels(selectors),
		client.GracePeriodSeconds(0),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		log.Error(err, "delete job error")
	}

	log.Warn("force delete pod")
	if err = c.DeleteAllOf(ctx,
		&corev1.Pod{},
		client.InNamespace(namespace),
		client.MatchingLabels(selectors),
		client.GracePeriodSeconds(0),
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil {
		log.Error(err, "delete pod error")
	}

	return errors.Wrapf(s2herrors.ErrForceDeletingComponents,
		"force cleaning up pods, namespace: %s, selectors: %+v", namespace, selectors)
}

func forceCleanupService(log s2hlog.Logger, c client.Client, services *corev1.ServiceList) error {
	ctx := context.Background()

	log.Warn("force delete service")
	for _, service := range services.Items {
		svc := service
		if err := c.Delete(ctx, &svc); err != nil {
			log.Error(err, fmt.Sprintf("delete service %s error", svc.Name))
		}
	}

	return errors.Wrapf(s2herrors.ErrForceDeletingComponents,
		"force cleaning up services")
}

func generateQueueHistoryName(queueName string) string {
	now := metav1.Now()
	return fmt.Sprintf("%s-%s", queueName, now.Format("20060102-150405"))
}
