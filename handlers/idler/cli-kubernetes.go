package idler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

// kubernetesCLI handles scaling CLI based deployments in kubernetes.
func (h *Handler) kubernetesCLI(ctx context.Context, opLog logr.Logger, namespace corev1.Namespace) {
	labelRequirements := generateLabelRequirements(h.Selectors.CLI.Builds)
	listOption := (&client.ListOptions{}).ApplyOptions([]client.ListOption{
		client.InNamespace(namespace.ObjectMeta.Name),
		client.MatchingLabelsSelector{
			Selector: labels.NewSelector().Add(labelRequirements...),
		},
	})

	builds := &corev1.PodList{}
	runningBuild := false
	if !h.Selectors.CLI.SkipBuildCheck {
		if err := h.Client.List(ctx, builds, listOption); err != nil {
			opLog.Error(err, fmt.Sprintf("Error getting running builds for namespace %s", namespace.ObjectMeta.Name))
		} else {
			for _, build := range builds.Items {
				if build.Status.Phase == "Running" {
					opLog.Info(fmt.Sprintf("Environment has running build, skipping"))
					runningBuild = true
					break
				}
			}
		}
	}
	// if there are no running builds, then check the cli pods
	if !runningBuild {
		// @TODO: eventually replace the `lagoon.sh/service=cli` check with `lagoon.sh/service-type=cli|cli-persistent` for better coverage
		labelRequirements := generateLabelRequirements(h.Selectors.CLI.Deployments)
		listOption = (&client.ListOptions{}).ApplyOptions([]client.ListOption{
			client.InNamespace(namespace.ObjectMeta.Name),
			client.MatchingLabelsSelector{
				Selector: labels.NewSelector().Add(labelRequirements...),
			},
		})
		deployments := &appsv1.DeploymentList{}
		if err := h.Client.List(ctx, deployments, listOption); err != nil {
			opLog.Error(err, fmt.Sprintf("Error getting deployments"))
		} else {
			for _, deployment := range deployments.Items {
				// if we have any services=cli, act on them
				zeroReps := new(int32)
				*zeroReps = 0
				if deployment.Spec.Replicas != zeroReps {
					opLog.Info(fmt.Sprintf("Deployment %s has %d running replicas", deployment.ObjectMeta.Name, *deployment.Spec.Replicas))
				} else {
					opLog.Info(fmt.Sprintf("Deployment %s is already idled", deployment.ObjectMeta.Name))
					break
				}
				if h.Debug {
					opLog.Info(fmt.Sprintf("Checking deployment %s for cronjobs", deployment.ObjectMeta.Name))
				}

				hasCrons := false
				if !h.Selectors.CLI.SkipCronCheck {
					for _, container := range deployment.Spec.Template.Spec.Containers {
						for _, env := range container.Env {
							if env.Name == "CRONJOBS" {
								if len(env.Value) > 0 {
									cronjobs := strings.Split(env.Value, `\n`)
									opLog.Info(fmt.Sprintf("Deployment %s has %d cronjobs defined", deployment.ObjectMeta.Name, len(cronjobs)))
									hasCrons = true
									break
								}
							}
						}
					}
				}
				if !hasCrons {
					pods := &corev1.PodList{}
					labelRequirements := generateLabelRequirements(h.Selectors.CLI.Pods)
					listOption = (&client.ListOptions{}).ApplyOptions([]client.ListOption{
						client.InNamespace(namespace.ObjectMeta.Name),
						client.MatchingLabelsSelector{
							Selector: labels.NewSelector().Add(labelRequirements...),
						},
					})
					if err := h.Client.List(ctx, pods, listOption); err != nil {
						opLog.Error(err, fmt.Sprintf("Error listing pods"))
					} else {
						for _, pod := range pods.Items {
							processCount := 0
							if !h.Selectors.CLI.SkipProcessCheck {
								if h.Debug {
									opLog.Info(fmt.Sprintf("Checking pod %s for running processes", pod.ObjectMeta.Name))
								}
								/*
									Anything running with parent PID0 is likely a user process
									/bin/bash -c "pgrep -P 0 | tail -n +3 | wc -l | tr -d ' '"
								*/
								var stdin io.Reader
								stdout, _, err := execPod(pod.ObjectMeta.Name, namespace.ObjectMeta.Name, []string{`/bin/sh`, `-c`, `pgrep -P 0|tail -n +3|wc -l|tr -d ' '`}, stdin, false)
								if err != nil {
									opLog.Error(err, fmt.Sprintf("Error when trying to exec to pod %s", pod.ObjectMeta.Name))
									break
								}
								trimmed := strings.TrimSpace(string(stdout))
								pcInt, err := strconv.Atoi(trimmed[len(trimmed)-1:])
								if err == nil {
									if pcInt > 0 {
										processCount = pcInt
									}
								}
								if processCount == 0 {
									opLog.Info(fmt.Sprintf("Pod %s has no running processes, idling", pod.ObjectMeta.Name))
								}
							}
							if processCount == 0 {
								if !h.DryRun {
									scaleDeployment := deployment.DeepCopy()
									mergePatch, _ := json.Marshal(map[string]interface{}{
										"spec": map[string]interface{}{
											"replicas": 0,
										},
									})
									if err := h.Client.Patch(ctx, scaleDeployment, client.RawPatch(types.MergePatchType, mergePatch)); err != nil {
										opLog.Error(err, fmt.Sprintf("Error scaling deployment %s", deployment.ObjectMeta.Name))
									} else {
										opLog.Info(fmt.Sprintf("Deployment %s scaled to 0", deployment.ObjectMeta.Name))
									}
								} else {
									opLog.Info(fmt.Sprintf("Deployment %s would be scaled to 0", deployment.ObjectMeta.Name))
								}
							}
						}
					}
				}
			}
		}
	}
}
