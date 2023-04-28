package idler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	client "sigs.k8s.io/controller-runtime/pkg/client"

	prometheusapiv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prometheusmodel "github.com/prometheus/common/model"
)

// kubernetesServices handles scaling deployments in kubernetes.
func (h *Handler) kubernetesServices(ctx context.Context, opLog logr.Logger, namespace corev1.Namespace, lagoonProject string, idleMinutes int, environmentType string) {
	labelRequirements := generateLabelRequirements(h.Selectors.Service.Builds)
	listOption := (&client.ListOptions{}).ApplyOptions([]client.ListOption{
		client.InNamespace(namespace.ObjectMeta.Name),
		client.MatchingLabelsSelector{
			Selector: labels.NewSelector().Add(labelRequirements...),
		},
	})
	builds := &corev1.PodList{}
	runningBuild := false
	if !h.Selectors.Service.SkipBuildCheck {
		if err := h.Client.List(ctx, builds, listOption); err != nil {
			opLog.Error(err, fmt.Sprintf("Error getting running builds for namespace %s", namespace.ObjectMeta.Name))
		} else {
			for _, build := range builds.Items {
				if build.Status.Phase == "Running" || build.Status.Phase == "Pending" {
					// if we have any pending builds, break out of this loop and try the next namespace
					opLog.Info(fmt.Sprintf("Environment has running build, skipping"))
					runningBuild = true
					break
				}
			}
		}
	}
	// if there are no builds, then check all the deployments that match our labelselectors
	if !runningBuild {
		labelRequirements := generateLabelRequirements(h.Selectors.Service.Deployments)
		listOption = (&client.ListOptions{}).ApplyOptions([]client.ListOption{
			client.InNamespace(namespace.ObjectMeta.Name),
			client.MatchingLabelsSelector{
				Selector: labels.NewSelector().Add(labelRequirements...),
			},
		})
		idle := false
		deployments := &appsv1.DeploymentList{}
		if err := h.Client.List(ctx, deployments, listOption); err != nil {
			// if we can't get any deployment configs for this namespace, log it and move on to the next
			opLog.Error(err, fmt.Sprintf("Error getting deployments"))
			return
		}
		// fmt.Println(labelRequirements)                 // TODO: remove
		// fmt.Println("deploys", len(deployments.Items)) // TODO: remove
		for _, deployment := range deployments.Items {
			checkPods := false
			zeroReps := new(int32)
			*zeroReps = 0
			if deployment.Spec.Replicas != zeroReps {
				opLog.Info(fmt.Sprintf("Deployment %s has %d running replicas", deployment.ObjectMeta.Name, *deployment.Spec.Replicas))
				checkPods = true
			} else {
				if h.Debug {
					opLog.Info(fmt.Sprintf("Deployment %s already idled", deployment.ObjectMeta.Name))
				}
			}
			if checkPods {
				pods := &corev1.PodList{}
				// pods in kubernetes have the label `h.Selectors.ServiceName` with the name of the deployment in it
				listOption = (&client.ListOptions{}).ApplyOptions([]client.ListOption{
					client.InNamespace(namespace.ObjectMeta.Name),
					client.MatchingLabels(map[string]string{h.Selectors.ServiceName: deployment.ObjectMeta.Name}),
				})
				// fmt.Println(listOption) // TODO: remove
				if err := h.Client.List(ctx, pods, listOption); err != nil {
					// if we can't get any pods for this deployment, log it and move on to the next
					opLog.Error(err, fmt.Sprintf("Error listing pods"))
					break
				}
				// fmt.Println("Pods", len(pods.Items)) // TODO: remove
				for _, pod := range pods.Items {
					// check if the runtime of the pod is more than our interval
					if pod.Status.StartTime != nil {
						hs := time.Now().Sub(pod.Status.StartTime.Time).Minutes()
						if h.Debug {
							opLog.Info(fmt.Sprintf("Pod %s has been running for %d minutes", pod.ObjectMeta.Name, int(hs)))
						}
						if int(hs) >= idleMinutes {
							// if it is, set the idle flag
							opLog.Info(fmt.Sprintf("Pod %s will be idled as it has been running for more than %d minutes", pod.ObjectMeta.Name, idleMinutes))
							idle = true
						}
					}
				}
			}
		}
		// we check the idle flag, then proceed to check the router logs and eventually idle the environment
		if idle {
			numHits := 0
			if !h.Selectors.Service.SkipHitCheck {
				opLog.Info(fmt.Sprintf("Environment marked for idling, checking routerlogs for hits"))
				// query prometheus for hits to ingress resources in this namespace
				v1api := prometheusapiv1.NewAPI(h.PrometheusClient)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				var timeRange string
				if environmentType == "production" {
					timeRange = fmt.Sprintf("%dm", idleMinutes)
				} else {
					timeRange = h.PrometheusCheckInterval
				}

				// get the number of requests to any ingress in the exported namespace by status code
				promQuery := fmt.Sprintf(
					"round(sum(increase(nginx_ingress_controller_requests{exported_namespace=\"%s\"}[%s])) by (status))",
					namespace.ObjectMeta.Name,
					timeRange,
					// h.PrometheusCheckInterval,
				)
				result, warnings, err := v1api.Query(ctx, promQuery, time.Now())
				if err != nil {
					fmt.Printf("Error querying Prometheus: %v\n", err)
					return
				}
				if len(warnings) > 0 {
					fmt.Printf("Warnings: %v\n", warnings)
				}
				// and then add up the results of all the status requests to determine hit count
				if result.Type() == prometheusmodel.ValVector {
					resultVal := result.(prometheusmodel.Vector)
					for _, elem := range resultVal {
						hits, _ := strconv.Atoi(elem.Value.String())
						numHits = numHits + hits
					}
				}
				// if the hits are not 0, then the environment doesn't need to be idled
				opLog.Info(fmt.Sprintf("Environment has had %d hits in the last %s", numHits, h.PrometheusCheckInterval))
				if numHits != 0 {
					opLog.Info(fmt.Sprintf("Environment does not need idling"))
					return
				}
			} else {
				opLog.Info(fmt.Sprintf("Environment marked for idling, ignoring the routerlogs for hits"))
			}
			// if there weren't any issues patching the ingress, then proceed to scale the deployments
			// just disregard the error, we're logging it in the patchIngres function, but if that step fails
			// the environment shouldn't be idled, as it will never unidle if the ingress annotation doesn't exist
			err := h.patchIngress(ctx, opLog, namespace)
			if err != nil {
				// if patching the ingress resources fail, then don't idle the environment
				opLog.Info(fmt.Sprintf("Environment not idled due to errors patching ingress"))
				return
			}
			opLog.Info(fmt.Sprintf("Environment will be idled"))
			h.idleDeployments(ctx, opLog, deployments)
		}
	}
}

func (h *Handler) idleDeployments(ctx context.Context, opLog logr.Logger, deployments *appsv1.DeploymentList) {
	d := []string{}
	for _, deployment := range deployments.Items {
		d = append(d, deployment.ObjectMeta.Name)
		// @TODO: use the patch method for the k8s client for now, this seems to work just fine
		// Patching the deployment also works as we patch the endpoints below
		if !h.DryRun {
			// to avoid having the idle replicas as 0, always use 1
			// this is to help prevent a deployment from incorrectly being told to have 0 replicas
			idleReplicas := new(int32)
			*idleReplicas = 1
			if *deployment.Spec.Replicas > 0 {
				// and override it with whatever is in the deployment if it is greater than 0
				idleReplicas = deployment.Spec.Replicas
			}
			scaleDeployment := deployment.DeepCopy()
			mergePatch, _ := json.Marshal(map[string]interface{}{
				"spec": map[string]interface{}{
					"replicas": 0,
				},
				"metadata": map[string]interface{}{
					"labels": map[string]string{
						// add the watch label so that the unidler knows to look at it
						"idling.amazee.io/watch": "true",
					},
					"annotations": map[string]string{
						// add these annotations so user knows to look at them
						"idling.amazee.io/idled-at":        time.Now().Format(time.RFC3339),
						"idling.amazee.io/unidle-replicas": strconv.FormatInt(int64(*idleReplicas), 10),
						"idling.amazee.io/idled":           "true",
					},
				},
			})
			if err := h.Client.Patch(ctx, scaleDeployment, client.RawPatch(types.MergePatchType, mergePatch)); err != nil {
				// log it but try and scale the rest of the deployments anyway (some idled is better than none?)
				opLog.Info(fmt.Sprintf("Error scaling deployment %s", deployment.ObjectMeta.Name))
			} else {
				opLog.Info(fmt.Sprintf("Deployment %s scaled to 0", deployment.ObjectMeta.Name))
			}
		} else {
			opLog.Info(fmt.Sprintf("Deployment %s would be scaled to 0", deployment.ObjectMeta.Name))
		}
	}
}

/*
patchIngress will patch any ingress with matching labels with the `custom-http-errors` annotation.
this annotation is used by the unidler to make sure that the correct information is passed to the custom backend for
the nginx ingress controller so that we can handle unidling of the environment properly
*/
func (h *Handler) patchIngress(ctx context.Context, opLog logr.Logger, namespace corev1.Namespace) error {
	if !h.Selectors.Service.SkipIngressPatch {
		labelRequirements := generateLabelRequirements(h.Selectors.Service.Ingress)
		listOption := (&client.ListOptions{}).ApplyOptions([]client.ListOption{
			client.InNamespace(namespace.ObjectMeta.Name),
			client.MatchingLabelsSelector{
				Selector: labels.NewSelector().Add(labelRequirements...),
			},
		})
		ingressList := &networkv1.IngressList{}
		if err := h.Client.List(ctx, ingressList, listOption); err != nil {
			// if we can't get any ingress for this namespace, log it and move on to the next
			opLog.Error(err, fmt.Sprintf("Error getting ingress"))
			return fmt.Errorf("Error getting ingress")
		}
		for _, ingress := range ingressList.Items {
			if !h.DryRun {
				ingressCopy := ingress.DeepCopy()
				mergePatch, _ := json.Marshal(map[string]interface{}{
					"metadata": map[string]interface{}{
						"annotations": map[string]string{
							// add the custom-http-errors annotation so that the unidler knows to handle this ingress
							"nginx.ingress.kubernetes.io/custom-http-errors": "503",
						},
					},
				})
				if err := h.Client.Patch(ctx, ingressCopy, client.RawPatch(types.MergePatchType, mergePatch)); err != nil {
					// log it but try and patch the other ingress anyway (some idled is better than none?)
					opLog.Info(fmt.Sprintf("Error patching ingress %s", ingress.ObjectMeta.Name))
					return fmt.Errorf(fmt.Sprintf("Error patching ingress %s", ingress.ObjectMeta.Name))
				}
				opLog.Info(fmt.Sprintf("Ingress %s patched", ingress.ObjectMeta.Name))
			} else {
				opLog.Info(fmt.Sprintf("Ingress %s would be patched", ingress.ObjectMeta.Name))
			}
		}
	}
	return nil
}
