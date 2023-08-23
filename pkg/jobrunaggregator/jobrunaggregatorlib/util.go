package jobrunaggregatorlib

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowjobclientset "k8s.io/test-infra/prow/client/clientset/versioned"
	prowjobinformers "k8s.io/test-infra/prow/client/informers/externalversions"
	"k8s.io/utils/clock"

	"github.com/openshift/ci-tools/pkg/jobrunaggregator/jobrunaggregatorapi"
	"github.com/openshift/ci-tools/pkg/junit"
)

const (
	JobStateQuerySourceBigQuery = "bigquery"
	JobStateQuerySourceCluster  = "cluster"
	prowJobJobNameAnnotation    = "prow.k8s.io/job"
	prowJobJobRunIDLabel        = "prow.k8s.io/build-id"
)

var (
	KnownQuerySources = sets.Set[string]{JobStateQuerySourceBigQuery: sets.Empty{}, JobStateQuerySourceCluster: sets.Empty{}}
)

type JobRunGetter interface {
	// GetRelatedJobRuns gets all related job runs for analysis
	GetRelatedJobRuns(ctx context.Context) ([]jobrunaggregatorapi.JobRunInfo, error)
}

type JobRunWaiter interface {
	// Wait waits until all job runs finish, or time out
	Wait(ctx context.Context) error
}

// WaitUntilTime waits until readAt time has passed
func WaitUntilTime(ctx context.Context, readyAt time.Time) error {
	logrus.Infof("Waiting now=%v, ReadyAt=%v.\n", time.Now(), readyAt)

	if time.Now().After(readyAt) {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(readyAt)):
		break
	}
	logrus.Infof("finished waiting until %v", readyAt)
	return nil
}

func getAllFinishedJobRuns(ctx context.Context, relatedJobRuns []jobrunaggregatorapi.JobRunInfo) ([]jobrunaggregatorapi.JobRunInfo, []jobrunaggregatorapi.JobRunInfo, []string, []string) {
	finishedJobRuns := []jobrunaggregatorapi.JobRunInfo{}
	unfinishedJobRuns := []jobrunaggregatorapi.JobRunInfo{}
	finishedJobRunNames := []string{}
	unfinishedJobRunNames := []string{}

	if len(relatedJobRuns) == 0 {
		return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames
	}

	for i := range relatedJobRuns {
		jobRun := relatedJobRuns[i]
		if !jobRun.IsFinished(ctx) {
			logrus.Debugf("%v/%v is not finished", jobRun.GetJobName(), jobRun.GetJobRunID())
			unfinishedJobRunNames = append(unfinishedJobRunNames, jobRun.GetJobRunID())
			unfinishedJobRuns = append(unfinishedJobRuns, jobRun)
			continue
		}

		prowJob, err := jobRun.GetProwJob(ctx)
		if err != nil {
			logrus.WithError(err).Errorf("error reading prowjob %v", jobRun.GetJobRunID())
			unfinishedJobRunNames = append(unfinishedJobRunNames, jobRun.GetJobRunID())
			unfinishedJobRuns = append(unfinishedJobRuns, jobRun)
			continue
		}

		if prowJob.Status.CompletionTime == nil {
			logrus.Debugf("%v/%v has no completion time for resourceVersion=%v", jobRun.GetJobName(), jobRun.GetJobRunID(), prowJob.ResourceVersion)
			unfinishedJobRunNames = append(unfinishedJobRunNames, jobRun.GetJobRunID())
			unfinishedJobRuns = append(unfinishedJobRuns, jobRun)
			continue
		}
		finishedJobRuns = append(finishedJobRuns, jobRun)
		finishedJobRunNames = append(finishedJobRunNames, jobRun.GetJobName()+jobRun.GetJobRunID())
	}
	return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames
}

type DefaultJobRunWaiter struct {
	JobRunGetter      JobRunGetter
	TimeToStopWaiting time.Time
}

func (w DefaultJobRunWaiter) Wait(ctx context.Context) error {
	clock := clock.RealClock{}
	relatedJobRuns, err := w.JobRunGetter.GetRelatedJobRuns(ctx)
	if err != nil {
		return err
	}

	for {
		fmt.Println() // for prettier logs

		_, _, _, unfinishedJobRunNames := getAllFinishedJobRuns(ctx, relatedJobRuns)

		// ready or not, it's time to check
		if clock.Now().After(w.TimeToStopWaiting) {
			logrus.Infof("waited long enough. Ready or not, here I come. (readyOrNot=%v now=%v)", w.TimeToStopWaiting, clock.Now())
			break
		}

		if len(unfinishedJobRunNames) > 0 {
			logrus.Infof("found %d unfinished related jobRuns: %v\n", len(unfinishedJobRunNames), strings.Join(unfinishedJobRunNames, ", "))
			select {
			case <-time.After(10 * time.Minute):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		break
	}
	return nil
}

type ClusterJobRunWaiter struct {
	ProwJobClient      *prowjobclientset.Clientset
	TimeToStopWaiting  time.Time
	ProwJobMatcherFunc ProwJobMatcherFunc
}

func (w ClusterJobRunWaiter) Wait(ctx context.Context) error {
	if w.ProwJobClient == nil {
		return fmt.Errorf("prowjob client is missing")
	}
	logrus.Infof("Going to wait until %+v with timeout value %+v", w.TimeToStopWaiting, time.Until(w.TimeToStopWaiting))

	prowJobInformerFactory := prowjobinformers.NewSharedInformerFactory(w.ProwJobClient, 24*time.Hour)
	prowJobInformer := prowJobInformerFactory.Prow().V1().ProwJobs()

	uncompletedJobMap := map[string]*prowv1.ProwJob{}
	wg := sync.WaitGroup{}
	count := 0
	prowJobInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				prowJob := obj.(*prowv1.ProwJob)
				if w.ProwJobMatcherFunc(prowJob) &&
					prowJob.Status.CompletionTime == nil {
					jobRunID := prowJob.Labels[prowJobJobRunIDLabel]
					if _, ok := uncompletedJobMap[jobRunID]; !ok {
						uncompletedJobMap[jobRunID] = prowJob
						wg.Add(1)
						count++
						logrus.Infof("AddFunc for job: %s, ID: %s, count: %d", prowJob.Labels[prowJobJobNameAnnotation], prowJob.Labels[prowJobJobRunIDLabel], count)
					}
				}
			},
			UpdateFunc: func(_, newObj interface{}) {
				prowJob := newObj.(*prowv1.ProwJob)
				if w.ProwJobMatcherFunc(prowJob) &&
					prowJob.Status.CompletionTime != nil {
					jobRunID := prowJob.Labels[prowJobJobRunIDLabel]
					if _, ok := uncompletedJobMap[jobRunID]; ok {
						delete(uncompletedJobMap, jobRunID)
						wg.Done()
						count--
						logrus.Infof("Update for job: %s, ID: %s, count: %d", prowJob.Labels[prowJobJobNameAnnotation], prowJob.Labels[prowJobJobRunIDLabel], count)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				prowJob := obj.(*prowv1.ProwJob)
				if w.ProwJobMatcherFunc(prowJob) {
					jobRunID := prowJob.Labels[prowJobJobRunIDLabel]
					if _, ok := uncompletedJobMap[jobRunID]; ok {
						delete(uncompletedJobMap, jobRunID)
						wg.Done()
						count--
						logrus.Infof("Delete for job: %s, ID: %s, count %d", prowJob.Labels[prowJobJobNameAnnotation], prowJob.Labels[prowJobJobRunIDLabel], count)
					}
				}
			},
		},
	)
	prowJobInformerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), prowJobInformer.Informer().HasSynced) {
		return fmt.Errorf("prowjob informer sync error")
	}
	done := make(chan error, 1)
	go func() {
		wg.Wait()
		done <- nil
	}()

	select {
	case <-time.After(time.Until(w.TimeToStopWaiting)):
		break
	case <-done:
		break
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// WaitAndGetAllFinishedJobRuns waits for all job runs to finish until timeToStopWaiting. It returns all finished and unfinished job runs
func WaitAndGetAllFinishedJobRuns(ctx context.Context,
	jobRunGetter JobRunGetter,
	waiter JobRunWaiter,
	outputDir string,
	variantInfo string) ([]jobrunaggregatorapi.JobRunInfo, []jobrunaggregatorapi.JobRunInfo, []string, []string, error) {
	finishedJobRuns := []jobrunaggregatorapi.JobRunInfo{}
	unfinishedJobRuns := []jobrunaggregatorapi.JobRunInfo{}
	finishedJobRunNames := []string{}
	unfinishedJobRunNames := []string{}

	err := waiter.Wait(ctx)
	if err != nil {
		return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames, err
	}

	// Refresh the job runs content one last time
	relatedJobRuns, err := jobRunGetter.GetRelatedJobRuns(ctx)
	if err != nil {
		return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames, err
	}

	finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames = getAllFinishedJobRuns(ctx, relatedJobRuns)

	summaryHTML := htmlForJobRuns(ctx, finishedJobRuns, unfinishedJobRuns, variantInfo)
	if err := os.WriteFile(filepath.Join(outputDir, "job-run-summary.html"), []byte(summaryHTML), 0644); err != nil {
		return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames, err
	}

	logrus.Infof("found %d finished jobRuns: %v and %d unfinished jobRuns: %v",
		len(finishedJobRunNames), strings.Join(finishedJobRunNames, ", "), len(unfinishedJobRunNames), strings.Join(unfinishedJobRunNames, ", "))
	return finishedJobRuns, unfinishedJobRuns, finishedJobRunNames, unfinishedJobRunNames, nil
}

// OutputTestCaseFailures prints detailed test failures
func OutputTestCaseFailures(parents []string, suite *junit.TestSuite) {
	currSuite := append(parents, suite.Name)
	for _, testCase := range suite.TestCases {
		if testCase.FailureOutput == nil {
			continue
		}
		if len(testCase.FailureOutput.Output) == 0 && len(testCase.FailureOutput.Message) == 0 {
			continue
		}
		fmt.Printf("Test Failed! suite=[%s], testCase=%v\nMessage: %v\n%v\n\n",
			strings.Join(currSuite, "  "),
			testCase.Name,
			testCase.FailureOutput.Message,
			testCase.SystemOut)
	}

	for _, child := range suite.Children {
		OutputTestCaseFailures(currSuite, child)
	}
}

func GetProwJobClient() (*prowjobclientset.Clientset, error) {
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	clusterConfig, err := cfg.ClientConfig()
	if err != nil {
		return nil, err
	}

	prowJobClient, err := prowjobclientset.NewForConfig(clusterConfig)
	if err != nil {
		return nil, err
	}
	return prowJobClient, nil
}
