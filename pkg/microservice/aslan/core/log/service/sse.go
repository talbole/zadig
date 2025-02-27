/*
Copyright 2021 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/kube"
	"github.com/koderover/zadig/pkg/setting"
	kubeclient "github.com/koderover/zadig/pkg/shared/kube/client"
	"github.com/koderover/zadig/pkg/tool/kube/containerlog"
	"github.com/koderover/zadig/pkg/tool/kube/getter"
	"github.com/koderover/zadig/pkg/tool/kube/label"
	"github.com/koderover/zadig/pkg/tool/kube/watcher"
)

const (
	timeout = 5 * time.Minute
)

type GetContainerOptions struct {
	Namespace     string
	PipelineName  string
	SubTask       string
	JobName       string
	JobType       string
	TailLines     int64
	TaskID        int64
	PipelineType  string
	ServiceName   string
	ServiceModule string
	TestName      string
	EnvName       string
	ProductName   string
	ClusterID     string
}

func ContainerLogStream(ctx context.Context, streamChan chan interface{}, envName, productName, podName, containerName string, follow bool, tailLines int64, log *zap.SugaredLogger) {
	productInfo, err := commonrepo.NewProductColl().Find(&commonrepo.ProductFindOptions{Name: productName, EnvName: envName})
	if err != nil {
		log.Errorf("kubeCli.GetContainerLogStream error: %v", err)
		return
	}
	clientset, err := kube.GetClientset(productInfo.ClusterID)
	if err != nil {
		log.Errorf("failed to find ns and kubeClient: %v", err)
		return
	}
	containerLogStream(ctx, streamChan, productInfo.Namespace, podName, containerName, follow, tailLines, clientset, log)
}

func containerLogStream(ctx context.Context, streamChan chan interface{}, namespace, podName, containerName string, follow bool, tailLines int64, client kubernetes.Interface, log *zap.SugaredLogger) {
	log.Infof("[GetContainerLogsSSE] Get container log of pod %s", podName)

	out, err := containerlog.GetContainerLogStream(ctx, namespace, podName, containerName, follow, tailLines, client)
	if err != nil {
		log.Errorf("kubeCli.GetContainerLogStream error: %v", err)
		return
	}
	defer func() {
		err := out.Close()
		if err != nil {
			log.Errorf("Failed to close container log stream, error: %v", err)
		}
	}()

	buf := bufio.NewReader(out)

	for {
		select {
		case <-ctx.Done():
			log.Infof("Connection is closed, container log stream stopped")
			return
		default:
			line, err := buf.ReadString('\n')
			if err == nil {
				line = strings.TrimSpace(line)
				streamChan <- line
			}
			if err == io.EOF {
				line = strings.TrimSpace(line)
				if len(line) > 0 {
					streamChan <- line
				}
				log.Infof("No more input is available, container log stream stopped")
				return
			}

			if err != nil {
				log.Errorf("scan container log stream error: %v", err)
				return
			}
		}
	}
}

func parseServiceName(fullServiceName, serviceModule string) (string, string) {
	// when service module is passed, use the passed value
	// otherwise we fall back to the old logic
	if len(serviceModule) > 0 {
		return strings.TrimPrefix(fullServiceName, serviceModule+"_"), serviceModule
	}
	var serviceName string
	serviceNames := strings.Split(fullServiceName, "_")
	switch len(serviceNames) {
	case 1:
		serviceModule = serviceNames[0]
	case 2:
		// Note: Starting from V1.10.0, this field will be in the format of `ServiceModule_ServiceName`.
		serviceModule = serviceNames[0]
		serviceName = serviceNames[1]
	}
	return serviceName, serviceModule
}

func TaskContainerLogStream(ctx context.Context, streamChan chan interface{}, options *GetContainerOptions, log *zap.SugaredLogger) {
	if options == nil {
		return
	}
	log.Debugf("Start to get task container log.")

	serviceName, serviceModule := parseServiceName(options.ServiceName, options.ServiceModule)

	// Cloud host scenario reads real-time logs from the environment, so pipelineName is empty.
	if options.EnvName != "" && options.ProductName != "" && options.PipelineName == "" {
		// Modify pipelineName to check whether pipelineName is empty:
		// - Empty pipelineName indicates requests from the environment
		// - Non-empty pipelineName indicate requests from workflow tasks
		options.PipelineName = fmt.Sprintf("%s-%s-%s", serviceName, options.EnvName, "job")
		if taskObj, err := commonrepo.NewTaskColl().FindTask(options.PipelineName, config.ServiceType); err == nil {
			options.TaskID = taskObj.TaskID
		}
	} else if options.ProductName != "" {
		buildFindOptions := &commonrepo.BuildFindOption{
			ProductName: options.ProductName,
			Targets:     []string{serviceModule},
		}
		if serviceName != "" {
			buildFindOptions.ServiceName = serviceName
		}

		build, err := commonrepo.NewBuildColl().Find(buildFindOptions)
		if err != nil {
			// Maybe this service is a shared service
			buildFindOptions := &commonrepo.BuildFindOption{
				Targets: []string{serviceModule},
			}
			if serviceName != "" {
				buildFindOptions.ServiceName = serviceName
			}

			build, err = commonrepo.NewBuildColl().Find(buildFindOptions)
			if err != nil {
				log.Errorf("Failed to query build for service %s: %s", serviceName, err)
				return
			}
		}
		// Compatible with the situation where the old data has not been modified
		if build != nil && build.PreBuild != nil && build.PreBuild.ClusterID != "" {
			options.ClusterID = build.PreBuild.ClusterID

			switch build.PreBuild.ClusterID {
			case setting.LocalClusterID:
				options.Namespace = config.Namespace()
			default:
				options.Namespace = setting.AttachedClusterNamespace
			}
		}
	}

	if options.SubTask == "" {
		options.SubTask = string(config.TaskBuild)
	}
	selector := labels.Set(label.GetJobLabels(&label.JobLabel{
		PipelineName: options.PipelineName,
		TaskID:       options.TaskID,
		TaskType:     options.SubTask,
		ServiceName:  options.ServiceName,
		PipelineType: options.PipelineType,
	})).AsSelector()
	waitAndGetLog(ctx, streamChan, selector, options, log)
}

func WorkflowTaskV4ContainerLogStream(ctx context.Context, streamChan chan interface{}, options *GetContainerOptions, log *zap.SugaredLogger) {
	if options == nil {
		return
	}
	log.Debugf("Start to get task container log.")
	task, err := commonrepo.NewworkflowTaskv4Coll().Find(options.PipelineName, options.TaskID)
	if err != nil {
		log.Errorf("Failed to find workflow %s taskID %s: %v", options.PipelineName, options.TaskID, err)
		return
	}
	for _, stage := range task.Stages {
		for _, job := range stage.Jobs {
			if job.Name != options.SubTask {
				continue
			}
			options.JobName = job.K8sJobName
			options.JobType = job.JobType
			switch job.JobType {
			case string(config.JobZadigBuild):
				fallthrough
			case string(config.JobFreestyle):
				fallthrough
			case string(config.JobZadigTesting):
				fallthrough
			case string(config.JobZadigScanning):
				fallthrough
			case string(config.JobZadigDistributeImage):
				fallthrough
			case string(config.JobBuild):
				jobSpec := &commonmodels.JobTaskFreestyleSpec{}
				if err := commonmodels.IToi(job.Spec, jobSpec); err != nil {
					log.Errorf("Failed to parse job spec: %v", err)
					return
				}
				options.ClusterID = jobSpec.Properties.ClusterID
			case string(config.JobPlugin):
				jobSpec := &commonmodels.JobTaskPluginSpec{}
				if err := commonmodels.IToi(job.Spec, jobSpec); err != nil {
					log.Errorf("Failed to parse job spec: %v", err)
					return
				}
				options.ClusterID = jobSpec.Properties.ClusterID
			default:
				log.Errorf("get real-time log error, unsupported job type %s", job.JobType)
				return
			}
			if options.ClusterID == "" {
				options.ClusterID = setting.LocalClusterID
			}
			switch options.ClusterID {
			case setting.LocalClusterID:
				options.Namespace = config.Namespace()
			default:
				options.Namespace = setting.AttachedClusterNamespace
			}
			break
		}
	}

	selector := getWorkflowSelector(options)
	waitAndGetLog(ctx, streamChan, selector, options, log)
}

func TestJobContainerLogStream(ctx context.Context, streamChan chan interface{}, options *GetContainerOptions, log *zap.SugaredLogger) {
	options.SubTask = string(config.TaskTestingV2)
	selector := labels.Set(label.GetJobLabels(&label.JobLabel{
		PipelineName: options.PipelineName,
		TaskID:       options.TaskID,
		TaskType:     options.SubTask,
		ServiceName:  options.ServiceName,
		PipelineType: options.PipelineType,
	})).AsSelector()
	// get cluster ID
	testing, _ := commonrepo.NewTestingColl().Find(getTestName(options.ServiceName), "")
	// Compatible with the situation where the old data has not been modified
	if testing != nil && testing.PreTest != nil && testing.PreTest.ClusterID != "" {
		options.ClusterID = testing.PreTest.ClusterID

		switch testing.PreTest.ClusterID {
		case setting.LocalClusterID:
			options.Namespace = config.Namespace()
		default:
			options.Namespace = setting.AttachedClusterNamespace
		}
	}

	waitAndGetLog(ctx, streamChan, selector, options, log)
}

func getTestName(serviceName string) string {
	testName := strings.TrimRight(serviceName, "-job")
	return testName
}

func waitAndGetLog(ctx context.Context, streamChan chan interface{}, selector labels.Selector, options *GetContainerOptions, log *zap.SugaredLogger) {
	PodCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Debugf("Waiting until pod is running before establishing the stream. labelSelector: %+v, clusterId: %s, namespace: %s", selector, options.ClusterID, options.Namespace)
	clientSet, err := kubeclient.GetClientset(config.HubServerAddress(), options.ClusterID)
	if err != nil {
		log.Errorf("GetContainerLogs, get client set error: %s", err)
		return
	}

	err = watcher.WaitUntilPodRunning(PodCtx, options.Namespace, selector, clientSet)
	if err != nil {
		log.Errorf("GetContainerLogs, wait pod running error: %s", err)
		return
	}

	kubeClient, err := kubeclient.GetKubeClient(config.HubServerAddress(), options.ClusterID)
	if err != nil {
		log.Errorf("GetContainerLogs, get kube client error: %s", err)
		return
	}

	pods, err := getter.ListPods(options.Namespace, selector, kubeClient)
	if err != nil {
		log.Errorf("GetContainerLogs, get pod error: %+v", err)
		return
	}

	log.Debugf("Found %d running pods", len(pods))

	if len(pods) > 0 {
		containerLogStream(
			ctx, streamChan,
			options.Namespace,
			pods[0].Name, options.SubTask,
			true,
			options.TailLines,
			clientSet,
			log,
		)
	}
}

func getWorkflowSelector(options *GetContainerOptions) labels.Selector {
	retMap := map[string]string{
		setting.JobLabelSTypeKey: strings.Replace(options.JobType, "_", "-", -1),
		setting.JobLabelNameKey:  strings.Replace(options.JobName, "_", "-", -1),
	}
	// no need to add labels with empty value to a job
	for k, v := range retMap {
		if len(v) == 0 {
			delete(retMap, k)
		}
	}
	return labels.Set(retMap).AsSelector()
}
