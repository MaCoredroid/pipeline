/*
Copyright 2018 The Knative Authors
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

package taskrun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/knative/build-pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/knative/build-pipeline/pkg/reconciler"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/taskrun/config"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/taskrun/resources"
	"github.com/knative/build-pipeline/pkg/system"
	"github.com/knative/build-pipeline/test"
	tb "github.com/knative/build-pipeline/test/builder"
	buildv1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	"github.com/knative/pkg/configmap"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekubeclientset "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const (
	entrypointLocation  = "/tools/entrypoint"
	toolsMountName      = "tools"
	buildNameLabelKey   = "build.knative.dev/buildName"
	taskRunNameLabelKey = "pipeline.knative.dev/taskRun"
)

var (
	ignoreLastTransitionTime = cmpopts.IgnoreTypes(duckv1alpha1.Condition{}.LastTransitionTime.Inner.Time)
	// Pods are created with a random 3-byte (6 hex character) suffix that we want to ignore in our diffs.
	ignoreRandomPodNameSuffix = cmp.FilterPath(func(path cmp.Path) bool {
		return path.GoString() == "{*v1.Pod}.ObjectMeta.Name"
	}, cmp.Comparer(func(name1, name2 string) bool {
		return name1[:len(name1)-6] == name2[:len(name2)-6]
	}))

	toolsMount = corev1.VolumeMount{
		Name:      toolsMountName,
		MountPath: "/tools",
	}
	workspaceDir                = "/workspace"
	implicitBuilderVolumeMounts = corev1.VolumeMount{
		Name:      "home",
		MountPath: "/builder/home",
	}
	entrypointCopyStep = tb.BuildStep("place-tools", config.DefaultEntrypointImage,
		tb.Command("/bin/cp"),
		tb.Args("/entrypoint", entrypointLocation),
		tb.VolumeMount(toolsMount),
	)

	entrypointOptionEnvVar = tb.EnvVar("ENTRYPOINT_OPTIONS", `{"args":["/mycmd"],"process_log":"/tools/process-log.txt","marker_file":"/tools/marker-file.txt"}`)

	simpleStep  = tb.Step("simple-step", "foo", tb.Command("/mycmd"))
	simpleTask  = tb.Task("test-task", "foo", tb.TaskSpec(simpleStep))
	clustertask = tb.ClusterTask("test-cluster-task", tb.ClusterTaskSpec(simpleStep))

	outputTask = tb.Task("test-output-task", "foo", tb.TaskSpec(
		simpleStep, tb.TaskInputs(
			tb.InputsResource(gitResource.Name, v1alpha1.PipelineResourceTypeGit),
			tb.InputsResource(anotherGitResource.Name, v1alpha1.PipelineResourceTypeGit),
		),
		tb.TaskOutputs(tb.OutputsResource(gitResource.Name, v1alpha1.PipelineResourceTypeGit)),
	))

	saTask = tb.Task("test-with-sa", "foo", tb.TaskSpec(tb.Step("sa-step", "foo", tb.Command("/mycmd"))))

	templatedTask = tb.Task("test-task-with-templating", "foo", tb.TaskSpec(
		tb.TaskInputs(
			tb.InputsResource("workspace", v1alpha1.PipelineResourceTypeGit),
			tb.InputsParam("myarg"), tb.InputsParam("myarghasdefault", tb.ParamDefault("dont see me")),
			tb.InputsParam("myarghasdefault2", tb.ParamDefault("thedefault")),
		),
		tb.TaskOutputs(tb.OutputsResource("myimage", v1alpha1.PipelineResourceTypeImage)),
		tb.Step("mycontainer", "myimage", tb.Command("/mycmd"), tb.Args(
			"--my-arg=${inputs.params.myarg}",
			"--my-arg-with-default=${inputs.params.myarghasdefault}",
			"--my-arg-with-default2=${inputs.params.myarghasdefault2}",
			"--my-additional-arg=${outputs.resources.myimage.url}",
		)),
		tb.Step("myothercontainer", "myotherimage", tb.Command("/mycmd"), tb.Args(
			"--my-other-arg=${inputs.resources.workspace.url}",
		)),
	))

	gitResource = tb.PipelineResource("git-resource", "foo", tb.PipelineResourceSpec(
		v1alpha1.PipelineResourceTypeGit, tb.PipelineResourceSpecParam("URL", "https://foo.git"),
	))
	anotherGitResource = tb.PipelineResource("another-git-resource", "foo", tb.PipelineResourceSpec(
		v1alpha1.PipelineResourceTypeGit, tb.PipelineResourceSpecParam("URL", "https://foobar.git"),
	))
	imageResource = tb.PipelineResource("image-resource", "foo", tb.PipelineResourceSpec(
		v1alpha1.PipelineResourceTypeImage, tb.PipelineResourceSpecParam("URL", "gcr.io/kristoff/sven"),
	))
)

func getToolsVolume(claimName string) corev1.Volume {
	return corev1.Volume{
		Name: toolsMountName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

func getRunName(tr *v1alpha1.TaskRun) string {
	return strings.Join([]string{tr.Namespace, tr.Name}, "/")
}

// getTaskRunController returns an instance of the TaskRun controller/reconciler that has been seeded with
// d, where d represents the state of the system (existing resources) needed for the test.
func getTaskRunController(d test.Data) test.TestAssets {
	c, i := test.SeedTestData(d)
	observer, logs := observer.New(zap.InfoLevel)
	configMapWatcher := configmap.NewInformedWatcher(c.Kube, system.Namespace)
	return test.TestAssets{
		Controller: NewController(
			reconciler.Options{
				Logger:            zap.New(observer).Sugar(),
				KubeClientSet:     c.Kube,
				PipelineClientSet: c.Pipeline,
				ConfigMapWatcher:  configMapWatcher,
			},
			i.TaskRun,
			i.Task,
			i.ClusterTask,
			i.PipelineResource,
			i.Pod,
		),
		Logs:      logs,
		Clients:   c,
		Informers: i,
	}
}

func TestReconcile(t *testing.T) {
	taskRunSuccess := tb.TaskRun("test-taskrun-run-success", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name, tb.TaskRefAPIVersion("a1")),
	))
	taskRunWithSaSuccess := tb.TaskRun("test-taskrun-with-sa-run-success", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef(saTask.Name, tb.TaskRefAPIVersion("a1")), tb.TaskRunServiceAccount("test-sa"),
	))
	taskRunTemplating := tb.TaskRun("test-taskrun-templating", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef(templatedTask.Name, tb.TaskRefAPIVersion("a1")),
		tb.TaskRunInputs(
			tb.TaskRunInputsParam("myarg", "foo"),
			tb.TaskRunInputsParam("myarghasdefault", "bar"),
			tb.TaskRunInputsResource("workspace", tb.TaskResourceBindingRef(gitResource.Name)),
		),
		tb.TaskRunOutputs(tb.TaskRunOutputsResource("myimage", tb.TaskResourceBindingRef("image-resource"))),
	))
	taskRunInputOutput := tb.TaskRun("test-taskrun-input-output", "foo",
		tb.TaskRunOwnerReference("PipelineRun", "test"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(outputTask.Name),
			tb.TaskRunInputs(
				tb.TaskRunInputsResource(gitResource.Name,
					tb.TaskResourceBindingRef(gitResource.Name),
					tb.TaskResourceBindingPaths("source-folder"),
				),
				tb.TaskRunInputsResource(anotherGitResource.Name,
					tb.TaskResourceBindingRef(anotherGitResource.Name),
					tb.TaskResourceBindingPaths("source-folder"),
				),
			),
			tb.TaskRunOutputs(
				tb.TaskRunOutputsResource(gitResource.Name,
					tb.TaskResourceBindingPaths("output-folder"),
				),
			),
		),
	)
	taskRunWithTaskSpec := tb.TaskRun("test-taskrun-with-taskspec", "foo", tb.TaskRunSpec(
		tb.TaskRunInputs(
			tb.TaskRunInputsParam("myarg", "foo"),
			tb.TaskRunInputsResource("workspace", tb.TaskResourceBindingRef(gitResource.Name)),
		),
		tb.TaskRunTaskSpec(
			tb.TaskInputs(
				tb.InputsResource("workspace", v1alpha1.PipelineResourceTypeGit),
				tb.InputsParam("myarg", tb.ParamDefault("mydefault")),
			),
			tb.Step("mycontainer", "myimage", tb.Command("/mycmd"),
				tb.Args("--my-arg=${inputs.params.myarg}"),
			),
		),
	))

	taskRunWithResourceSpecAndTaskSpec := tb.TaskRun("test-taskrun-with-resource-spec", "foo", tb.TaskRunSpec(
		tb.TaskRunInputs(
			tb.TaskRunInputsResource("workspace", tb.TaskResourceBindingResourceSpec(&v1alpha1.PipelineResourceSpec{
				Type: v1alpha1.PipelineResourceTypeGit,
				Params: []v1alpha1.Param{{
					Name:  "URL",
					Value: "github.com/build-pipeline.git",
				}, {
					Name:  "revision",
					Value: "rel-can",
				}},
			})),
		),
		tb.TaskRunTaskSpec(
			tb.TaskInputs(
				tb.InputsResource("workspace", v1alpha1.PipelineResourceTypeGit)),
			tb.Step("mystep", "ubuntu", tb.Command("mycmd")),
		),
	))

	taskRunWithClusterTask := tb.TaskRun("test-taskrun-with-cluster-task", "foo",
		tb.TaskRunSpec(tb.TaskRunTaskRef(clustertask.Name, tb.TaskRefKind(v1alpha1.ClusterTaskKind))),
	)

	taskRunWithLabels := tb.TaskRun("test-taskrun-with-labels", "foo",
		tb.TaskRunLabel("TaskRunLabel", "TaskRunValue"),
		tb.TaskRunLabel(buildNameLabelKey, "WillNotBeUsed"),
		tb.TaskRunLabel(taskRunNameLabelKey, "WillNotBeUsed"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
		),
	)

	taskruns := []*v1alpha1.TaskRun{
		taskRunSuccess, taskRunWithSaSuccess,
		taskRunTemplating, taskRunInputOutput,
		taskRunWithTaskSpec, taskRunWithClusterTask, taskRunWithResourceSpecAndTaskSpec,
		taskRunWithLabels,
	}

	d := test.Data{
		TaskRuns:          taskruns,
		Tasks:             []*v1alpha1.Task{simpleTask, saTask, templatedTask, outputTask},
		ClusterTasks:      []*v1alpha1.ClusterTask{clustertask},
		PipelineResources: []*v1alpha1.PipelineResource{gitResource, anotherGitResource, imageResource},
	}
	for _, tc := range []struct {
		name      string
		taskRun   *v1alpha1.TaskRun
		wantBuild *buildv1alpha1.Build
	}{{
		name:    "success",
		taskRun: taskRunSuccess,
		wantBuild: tb.Build("test-taskrun-run-success", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-run-success"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-run-success"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-run-success",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				entrypointCopyStep,
				tb.BuildStep("simple-step", "foo", tb.Command(entrypointLocation),
					entrypointOptionEnvVar, tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunSuccess.Name)),
			),
		),
	}, {
		name:    "serviceaccount",
		taskRun: taskRunWithSaSuccess,
		wantBuild: tb.Build("test-taskrun-with-sa-run-success", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-with-sa-run-success"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-with-sa-run-success"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-with-sa-run-success",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				tb.BuildServiceAccountName("test-sa"),
				entrypointCopyStep,
				tb.BuildStep("sa-step", "foo", tb.Command(entrypointLocation),
					entrypointOptionEnvVar, tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunWithSaSuccess.Name)),
			),
		),
	}, {
		name:    "params",
		taskRun: taskRunTemplating,
		wantBuild: tb.Build("test-taskrun-templating", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-templating"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-templating"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-templating",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				tb.BuildStep("git-source-git-resource", "override-with-git:latest",
					tb.Args("-url", "https://foo.git", "-revision", "master", "-path", "/workspace/workspace"),
					tb.VolumeMount(corev1.VolumeMount{
						Name:      "workspace",
						MountPath: workspaceDir,
					}),
					tb.VolumeMount(implicitBuilderVolumeMounts),
				),
				entrypointCopyStep,
				tb.BuildStep("mycontainer", "myimage", tb.Command(entrypointLocation),
					tb.EnvVar("ENTRYPOINT_OPTIONS", `{"args":["/mycmd","--my-arg=foo","--my-arg-with-default=bar","--my-arg-with-default2=thedefault","--my-additional-arg=gcr.io/kristoff/sven"],"process_log":"/tools/process-log.txt","marker_file":"/tools/marker-file.txt"}`),
					tb.VolumeMount(toolsMount),
				),
				tb.BuildStep("myothercontainer", "myotherimage", tb.Command(entrypointLocation),
					tb.EnvVar("ENTRYPOINT_OPTIONS", `{"args":["/mycmd","--my-other-arg=https://foo.git"],"process_log":"/tools/process-log.txt","marker_file":"/tools/marker-file.txt"}`),
					tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunTemplating.Name)),
			),
		),
	}, {
		name:    "wrap-steps",
		taskRun: taskRunInputOutput,
		wantBuild: tb.Build("test-taskrun-input-output", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-input-output"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-input-output"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-input-output",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				tb.BuildStep("create-dir-another-git-resource", "override-with-bash-noop:latest",
					tb.Args("-args", "mkdir -p /workspace/another-git-resource"),
				),
				tb.BuildStep("source-copy-another-git-resource-0", "override-with-bash-noop:latest",
					tb.Args("-args", "cp -r source-folder/. /workspace/another-git-resource"),
					tb.VolumeMount(corev1.VolumeMount{Name: "test-pvc", MountPath: "/pvc"}),
				),
				tb.BuildStep("create-dir-git-resource", "override-with-bash-noop:latest",
					tb.Args("-args", "mkdir -p /workspace/git-resource"),
				),
				tb.BuildStep("source-copy-git-resource-0", "override-with-bash-noop:latest",
					tb.Args("-args", "cp -r source-folder/. /workspace/git-resource"),
					tb.VolumeMount(corev1.VolumeMount{Name: "test-pvc", MountPath: "/pvc"}),
				),
				entrypointCopyStep,
				tb.BuildStep("simple-step", "foo", tb.Command(entrypointLocation),
					entrypointOptionEnvVar, tb.VolumeMount(toolsMount),
				),
				tb.BuildStep("source-mkdir-git-resource", "override-with-bash-noop:latest",
					tb.Args("-args", "mkdir -p output-folder"),
					tb.VolumeMount(corev1.VolumeMount{Name: "test-pvc", MountPath: "/pvc"}),
				),
				tb.BuildStep("source-copy-git-resource", "override-with-bash-noop:latest",
					tb.Args("-args", "cp -r /workspace/git-resource/. output-folder"),
					tb.VolumeMount(corev1.VolumeMount{Name: "test-pvc", MountPath: "/pvc"}),
				),
				tb.BuildVolume(getToolsVolume(taskRunInputOutput.Name)),
				tb.BuildVolume(resources.GetPVCVolume(taskRunInputOutput.GetPipelineRunPVCName())),
			),
		),
	}, {
		name:    "taskrun-with-taskspec",
		taskRun: taskRunWithTaskSpec,
		wantBuild: tb.Build("test-taskrun-with-taskspec", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-with-taskspec"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-with-taskspec"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-with-taskspec",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				tb.BuildStep("git-source-git-resource", "override-with-git:latest",
					tb.Args("-url", "https://foo.git", "-revision", "master", "-path", "/workspace/workspace"),
					tb.VolumeMount(corev1.VolumeMount{
						Name:      "workspace",
						MountPath: workspaceDir,
					}),
					tb.VolumeMount(implicitBuilderVolumeMounts),
				),
				entrypointCopyStep,
				tb.BuildStep("mycontainer", "myimage", tb.Command(entrypointLocation),
					tb.EnvVar("ENTRYPOINT_OPTIONS", `{"args":["/mycmd","--my-arg=foo"],"process_log":"/tools/process-log.txt","marker_file":"/tools/marker-file.txt"}`),
					tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunWithTaskSpec.Name)),
			),
		),
	}, {
		name:    "success-with-cluster-task",
		taskRun: taskRunWithClusterTask,
		wantBuild: tb.Build("test-taskrun-with-cluster-task", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-with-cluster-task"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-with-cluster-task"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-with-cluster-task",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(entrypointCopyStep,
				tb.BuildStep("simple-step", "foo", tb.Command(entrypointLocation),
					entrypointOptionEnvVar, tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunWithClusterTask.Name)),
			),
		),
	}, {
		name:    "taskrun-with-resource-spec-task-spec",
		taskRun: taskRunWithResourceSpecAndTaskSpec,
		wantBuild: tb.Build("test-taskrun-with-resource-spec", "foo",
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-with-resource-spec"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-with-resource-spec"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-with-resource-spec",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				tb.BuildStep("git-source-workspace", "override-with-git:latest",
					tb.Args("-url", "github.com/build-pipeline.git", "-revision", "rel-can", "-path", "/workspace/workspace"),
					tb.VolumeMount(corev1.VolumeMount{
						Name:      "workspace",
						MountPath: workspaceDir,
					}),
					tb.VolumeMount(implicitBuilderVolumeMounts),
				),
				entrypointCopyStep,
				tb.BuildStep("mystep", "ubuntu", tb.Command(entrypointLocation),
					tb.EnvVar("ENTRYPOINT_OPTIONS", `{"args":["mycmd"],"process_log":"/tools/process-log.txt","marker_file":"/tools/marker-file.txt"}`),
					tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunWithResourceSpecAndTaskSpec.Name)),
			),
		),
	}, {
		name:    "taskrun-with-labels",
		taskRun: taskRunWithLabels,
		wantBuild: tb.Build("test-taskrun-with-labels", "foo",
			tb.BuildLabel("TaskRunLabel", "TaskRunValue"),
			tb.BuildLabel(buildNameLabelKey, "test-taskrun-with-labels"),
			tb.BuildLabel(taskRunNameLabelKey, "test-taskrun-with-labels"),
			tb.BuildOwnerReference("TaskRun", "test-taskrun-with-labels",
				tb.OwnerReferenceAPIVersion("pipeline.knative.dev/v1alpha1")),
			tb.BuildSpec(
				entrypointCopyStep,
				tb.BuildStep("simple-step", "foo", tb.Command(entrypointLocation),
					entrypointOptionEnvVar, tb.VolumeMount(toolsMount),
				),
				tb.BuildVolume(getToolsVolume(taskRunWithLabels.Name)),
			),
		),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			testAssets := getTaskRunController(d)
			c := testAssets.Controller
			clients := testAssets.Clients
			saName := tc.taskRun.Spec.ServiceAccount
			if saName == "" {
				saName = "default"
			}
			clients.Kube.CoreV1().ServiceAccounts(tc.taskRun.Namespace).Create(&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      saName,
					Namespace: tc.taskRun.Namespace,
				},
			})

			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
				t.Errorf("expected no error. Got error %v", err)
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Errorf("Expected actions to be logged in the kubeclient, got none")
			}

			namespace, name, err := cache.SplitMetaNamespaceKey(tc.taskRun.Name)
			if err != nil {
				t.Errorf("Invalid resource key: %v", err)
			}

			tr, err := clients.Pipeline.PipelineV1alpha1().TaskRuns(namespace).Get(name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting updated taskrun: %v", err)
			}
			condition := tr.Status.GetCondition(duckv1alpha1.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionUnknown {
				t.Errorf("Expected invalid TaskRun to have in progress status, but had %v", condition)
			}
			if condition != nil && condition.Reason != reasonRunning {
				t.Errorf("Expected reason %q but was %s", reasonRunning, condition.Reason)
			}

			if tr.Status.PodName == "" {
				t.Fatalf("Reconcile didn't set pod name")
			}

			pod, err := clients.Kube.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to fetch build pod: %v", err)
			}
			// TODO: Using MakePod means that a diff will not catch issues
			// specific to the Build to Pod translation (e.g. if labels are
			// not propagated in MakePod). To avoid this issue we should create
			// a builder for Pods and use that intead.
			wantPod, err := resources.MakePod(tc.wantBuild, clients.Kube)
			if err != nil {
				t.Fatalf("MakePod: %v", err)
			}

			if d := cmp.Diff(pod, wantPod, ignoreRandomPodNameSuffix); d != "" {
				t.Errorf("pod doesn't match, diff: %s", d)
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Fatalf("Expected actions to be logged in the kubeclient, got none")
			}
		})
	}
}

func TestReconcile_InvalidTaskRuns(t *testing.T) {
	noTaskRun := tb.TaskRun("notaskrun", "foo", tb.TaskRunSpec(tb.TaskRunTaskRef("notask")))
	withWrongRef := tb.TaskRun("taskrun-with-wrong-ref", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef("taskrun-with-wrong-ref", tb.TaskRefKind(v1alpha1.ClusterTaskKind)),
	))
	taskRuns := []*v1alpha1.TaskRun{noTaskRun, withWrongRef}
	tasks := []*v1alpha1.Task{simpleTask}

	d := test.Data{
		TaskRuns: taskRuns,
		Tasks:    tasks,
	}

	testcases := []struct {
		name    string
		taskRun *v1alpha1.TaskRun
		reason  string
	}{
		{
			name:    "task run with no task",
			taskRun: noTaskRun,
			reason:  reasonFailedResolution,
		},
		{
			name:    "task run with no task",
			taskRun: withWrongRef,
			reason:  reasonFailedResolution,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testAssets := getTaskRunController(d)
			c := testAssets.Controller
			clients := testAssets.Clients
			err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun))
			// When a TaskRun is invalid and can't run, we don't want to return an error because
			// an error will tell the Reconciler to keep trying to reconcile; instead we want to stop
			// and forget about the Run.
			if err != nil {
				t.Errorf("Did not expect to see error when reconciling invalid TaskRun but saw %q", err)
			}
			if len(clients.Kube.Actions()) != 0 {
				t.Errorf("expected no actions created by the reconciler, got %v", clients.Kube.Actions())
			}
			// Since the TaskRun is invalid, the status should say it has failed
			condition := tc.taskRun.Status.GetCondition(duckv1alpha1.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionFalse {
				t.Errorf("Expected invalid TaskRun to have failed status, but had %v", condition)
			}
			if condition != nil && condition.Reason != tc.reason {
				t.Errorf("Expected failure to be because of reason %q but was %s", tc.reason, condition.Reason)
			}
		})
	}

}

func TestReconcileBuildFetchError(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-success", "foo",
		tb.TaskRunSpec(tb.TaskRunTaskRef("test-task")),
		tb.TaskRunStatus(tb.PodName("will-not-be-found")),
	)
	d := test.Data{
		TaskRuns: []*v1alpha1.TaskRun{taskRun},
		Tasks:    []*v1alpha1.Task{simpleTask},
	}

	testAssets := getTaskRunController(d)
	c := testAssets.Controller
	clients := testAssets.Clients

	clients.Kube.PrependReactor("*", "*", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		if action.GetVerb() == "get" && action.GetResource().Resource == "pods" {
			// handled fetching pods
			return true, nil, fmt.Errorf("induce failure fetching pods")
		}
		return false, nil, nil
	})

	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err == nil {
		t.Fatal("expected error when reconciling a Task for which we couldn't get the corresponding Build Pod but got nil")
	}
}

func TestReconcileBuildUpdateStatus(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-success", "foo", tb.TaskRunSpec(tb.TaskRunTaskRef("test-task")))
	build := &buildv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskRun.Name,
			Namespace: taskRun.Namespace,
		},
		Spec: *simpleTask.Spec.GetBuildSpec(),
	}
	// TODO(jasonhall): This avoids a circular dependency where
	// getTaskRunController takes a test.Data which must be populated with
	// a pod created from MakePod which requires a (fake) Kube client. When
	// we remove Build entirely from this controller, we should simply
	// specify the Pod we want to exist directly, and not call MakePod from
	// the build. This will break the cycle and allow us to simply use
	// clients normally.
	pod, err := resources.MakePod(build, fakekubeclientset.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: taskRun.Namespace,
		},
	}))
	if err != nil {
		t.Fatalf("MakePod: %v", err)
	}
	taskRun.Status = v1alpha1.TaskRunStatus{
		PodName: pod.Name,
	}
	d := test.Data{
		TaskRuns: []*v1alpha1.TaskRun{taskRun},
		Tasks:    []*v1alpha1.Task{simpleTask},
		Pods:     []*corev1.Pod{pod},
	}

	testAssets := getTaskRunController(d)
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err != nil {
		t.Fatalf("Unexpected error when Reconcile() : %v", err)
	}
	newTr, err := clients.Pipeline.PipelineV1alpha1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	if d := cmp.Diff(newTr.Status.GetCondition(duckv1alpha1.ConditionSucceeded), &duckv1alpha1.Condition{
		Type:    duckv1alpha1.ConditionSucceeded,
		Status:  corev1.ConditionUnknown,
		Message: "Running",
		Reason:  "Running",
	}, ignoreLastTransitionTime); d != "" {
		t.Fatalf("-got, +want: %v", d)
	}

	// update pod status and trigger reconcile : build is completed
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodSucceeded,
	}
	if _, err := clients.Kube.CoreV1().Pods(taskRun.Namespace).Update(pod); err != nil {
		t.Errorf("Unexpected error while updating build: %v", err)
	}
	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err != nil {
		t.Fatalf("Unexpected error when Reconcile(): %v", err)
	}

	newTr, err = clients.Pipeline.PipelineV1alpha1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error fetching taskrun: %v", err)
	}
	if d := cmp.Diff(newTr.Status.GetCondition(duckv1alpha1.ConditionSucceeded), &duckv1alpha1.Condition{
		Type:   duckv1alpha1.ConditionSucceeded,
		Status: corev1.ConditionTrue,
	}, ignoreLastTransitionTime); d != "" {
		t.Errorf("Taskrun Status diff -got, +want: %v", d)
	}
}

func TestUpdateStatusFromBuildStatus(t *testing.T) {
	completed := corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "success"},
	}
	waiting := corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{
			Message: "foo",
			Reason:  "bar",
		},
	}
	failed := corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 127, Reason: "oh-my-lord"},
	}
	startTime := metav1.NewTime(time.Date(2018, time.November, 10, 23, 0, 0, 0, time.UTC))
	completionTime := metav1.NewTime(time.Date(2018, time.November, 10, 23, 8, 0, 0, time.UTC))
	for _, tc := range []struct {
		name           string
		buildStatus    buildv1alpha1.BuildStatus
		expectedStatus v1alpha1.TaskRunStatus
	}{{
		name:        "empty build status",
		buildStatus: buildv1alpha1.BuildStatus{},
		expectedStatus: v1alpha1.TaskRunStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionUnknown,
				Reason:  reasonRunning,
				Message: reasonRunning,
			}},
			Steps: []v1alpha1.StepState{},
		},
	}, {
		name: "build validate failed",
		buildStatus: buildv1alpha1.BuildStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "BuildValidationFailed",
				Message: `serviceaccounts "missing-sa" not-found`,
			}},
		},
		expectedStatus: v1alpha1.TaskRunStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "BuildValidationFailed",
				Message: `serviceaccounts "missing-sa" not-found`,
			}},
			Steps: []v1alpha1.StepState{},
		},
	}, {
		name: "running build status",
		buildStatus: buildv1alpha1.BuildStatus{
			StartTime: &startTime,
			StepStates: []corev1.ContainerState{
				waiting,
			},
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Reason:  "Running build",
				Message: "Running build",
			}},
			Cluster: &buildv1alpha1.ClusterSpec{
				Namespace: "default",
				PodName:   "im-am-the-pod",
			},
		},
		expectedStatus: v1alpha1.TaskRunStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Reason:  "Running build",
				Message: "Running build",
			}},
			Steps: []v1alpha1.StepState{
				{ContainerState: *waiting.DeepCopy()},
			},
			PodName:   "im-am-the-pod",
			StartTime: &startTime,
		},
	}, {
		name: "completed build status (success)",
		buildStatus: buildv1alpha1.BuildStatus{
			StartTime:      &startTime,
			CompletionTime: &completionTime,
			StepStates: []corev1.ContainerState{
				completed,
			},
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionTrue,
				Reason:  "Build succeeded",
				Message: "Build succeeded",
			}},
			Cluster: &buildv1alpha1.ClusterSpec{
				Namespace: "default",
				PodName:   "im-am-the-pod",
			},
		},
		expectedStatus: v1alpha1.TaskRunStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionTrue,
				Reason:  "Build succeeded",
				Message: "Build succeeded",
			}},
			Steps: []v1alpha1.StepState{
				{ContainerState: *completed.DeepCopy()},
			},
			PodName:        "im-am-the-pod",
			StartTime:      &startTime,
			CompletionTime: &completionTime,
		},
	}, {
		name: "completed build status (failure)",
		buildStatus: buildv1alpha1.BuildStatus{
			StartTime:      &startTime,
			CompletionTime: &completionTime,
			StepStates: []corev1.ContainerState{
				completed,
				failed,
			},
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "Build failed",
				Message: "Build failed",
			}},
			Cluster: &buildv1alpha1.ClusterSpec{
				Namespace: "default",
				PodName:   "im-am-the-pod",
			},
		},
		expectedStatus: v1alpha1.TaskRunStatus{
			Conditions: []duckv1alpha1.Condition{{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "Build failed",
				Message: "Build failed",
			}},
			Steps: []v1alpha1.StepState{
				{ContainerState: *completed.DeepCopy()},
				{ContainerState: *failed.DeepCopy()},
			},
			PodName:        "im-am-the-pod",
			StartTime:      &startTime,
			CompletionTime: &completionTime,
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			taskRun := &v1alpha1.TaskRun{}
			updateStatusFromBuildStatus(taskRun, tc.buildStatus)
			if d := cmp.Diff(taskRun.Status, tc.expectedStatus, ignoreLastTransitionTime); d != "" {
				t.Errorf("-want, +got: %v", d)
			}
		})
	}
}

func TestCreateRedirectedBuild(t *testing.T) {
	cfg := &config.Config{
		Entrypoint: &config.Entrypoint{
			Image: config.DefaultEntrypointImage,
		},
	}
	ctx := config.ToContext(context.Background(), cfg)

	tr := tb.TaskRun("tr", "tr", tb.TaskRunSpec(
		tb.TaskRunServiceAccount("sa"),
	))
	bs := tb.Build("tr", "tr", tb.BuildSpec(
		tb.BuildStep("foo1", "bar1", tb.Command("abcd"), tb.Args("efgh")),
		tb.BuildStep("foo2", "bar2", tb.Command("abcd"), tb.Args("efgh")),
		tb.BuildVolume(corev1.Volume{Name: "v"}),
	)).Spec

	expectedSteps := len(bs.Steps) + 1
	expectedVolumes := len(bs.Volumes) + 1

	b, err := createRedirectedBuild(ctx, &bs, tr)
	if err != nil {
		t.Errorf("expected createRedirectedBuild to pass: %v", err)
	}
	if b.Name != tr.Name {
		t.Errorf("names do not match: %s should be %s", b.Name, tr.Name)
	}
	if len(b.Spec.Steps) != expectedSteps {
		t.Errorf("step counts do not match: %d should be %d", len(b.Spec.Steps), expectedSteps)
	}
	if len(b.Spec.Volumes) != expectedVolumes {
		t.Errorf("volumes do not match: %d should be %d", len(b.Spec.Volumes), expectedVolumes)
	}
	if b.Spec.ServiceAccountName != tr.Spec.ServiceAccount {
		t.Errorf("services accounts do not match: %s should be %s", b.Spec.ServiceAccountName, tr.Spec.ServiceAccount)
	}
}

func TestReconcileOnCompletedTaskRun(t *testing.T) {
	taskSt := &duckv1alpha1.Condition{
		Type:    duckv1alpha1.ConditionSucceeded,
		Status:  corev1.ConditionTrue,
		Reason:  "Build succeeded",
		Message: "Build succeeded",
	}
	taskRun := tb.TaskRun("test-taskrun-run-success", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name),
	), tb.TaskRunStatus(tb.Condition(*taskSt)))
	d := test.Data{
		TaskRuns: []*v1alpha1.TaskRun{
			taskRun,
		},
		Tasks: []*v1alpha1.Task{simpleTask},
	}

	testAssets := getTaskRunController(d)
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err != nil {
		t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
	}
	newTr, err := clients.Pipeline.PipelineV1alpha1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	if d := cmp.Diff(newTr.Status.GetCondition(duckv1alpha1.ConditionSucceeded), taskSt, ignoreLastTransitionTime); d != "" {
		t.Fatalf("-want, +got: %v", d)
	}
}

func TestReconcileOnCancelledTaskRun(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-cancelled", "foo", tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name),
		tb.TaskRunCancelled,
	), tb.TaskRunStatus(tb.Condition(duckv1alpha1.Condition{
		Type:   duckv1alpha1.ConditionSucceeded,
		Status: corev1.ConditionUnknown,
	})))
	d := test.Data{
		TaskRuns: []*v1alpha1.TaskRun{taskRun},
		Tasks:    []*v1alpha1.Task{simpleTask},
	}

	testAssets := getTaskRunController(d)
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err != nil {
		t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
	}
	newTr, err := clients.Pipeline.PipelineV1alpha1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	expectedStatus := &duckv1alpha1.Condition{
		Type:    duckv1alpha1.ConditionSucceeded,
		Status:  corev1.ConditionFalse,
		Reason:  "TaskRunCancelled",
		Message: `TaskRun "test-taskrun-run-cancelled" was cancelled`,
	}
	if d := cmp.Diff(newTr.Status.GetCondition(duckv1alpha1.ConditionSucceeded), expectedStatus, ignoreLastTransitionTime); d != "" {
		t.Fatalf("-want, +got: %v", d)
	}
}

func TestReconcileOnTimedOutTaskRun(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-timeout", "foo",
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
			tb.TaskRunTimeout(10*time.Second),
		),
		tb.TaskRunStatus(tb.Condition(duckv1alpha1.Condition{
			Type:   duckv1alpha1.ConditionSucceeded,
			Status: corev1.ConditionUnknown}),
			tb.TaskRunStartTime(time.Now().Add(-15*time.Second))))

	d := test.Data{
		TaskRuns: []*v1alpha1.TaskRun{taskRun},
		Tasks:    []*v1alpha1.Task{simpleTask},
	}

	testAssets := getTaskRunController(d)
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), fmt.Sprintf("%s/%s", taskRun.Namespace, taskRun.Name)); err != nil {
		t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
	}
	newTr, err := clients.Pipeline.PipelineV1alpha1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	expectedStatus := &duckv1alpha1.Condition{
		Type:    duckv1alpha1.ConditionSucceeded,
		Status:  corev1.ConditionFalse,
		Reason:  "TaskRunTimeout",
		Message: `TaskRun "test-taskrun-timeout" failed to finish within "10s"`,
	}
	if d := cmp.Diff(newTr.Status.GetCondition(duckv1alpha1.ConditionSucceeded), expectedStatus, ignoreLastTransitionTime); d != "" {
		t.Fatalf("-want, +got: %v", d)
	}
}
