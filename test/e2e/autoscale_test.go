// +build e2e

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

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pkgTest "github.com/knative/pkg/test"
	"github.com/knative/pkg/test/logging"
	"github.com/knative/serving/test"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	autoscaleExpectedOutput = "399989"
)

var (
	initialScaleToZeroThreshold   string
	initialScaleToZeroGracePeriod string
)

func isDeploymentScaledUp() func(d *v1beta1.Deployment) (bool, error) {
	return func(d *v1beta1.Deployment) (bool, error) {
		return d.Status.ReadyReplicas > 1, nil
	}
}

func isDeploymentScaledToZero() func(d *v1beta1.Deployment) (bool, error) {
	return func(d *v1beta1.Deployment) (bool, error) {
		return d.Status.ReadyReplicas == 0, nil
	}
}

func generateTraffic(clients *test.Clients, logger *logging.BaseLogger, concurrency int, duration time.Duration, domain string) error {
	totalRequests := make(chan struct{}, concurrency)
	successfulRequests := make(chan struct{}, concurrency)

	logger.Infof("Maintaining %d concurrent requests for %v.", concurrency, duration)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := time.After(duration)
			client, err := pkgTest.NewSpoofingClient(clients.KubeClient, logger, domain, test.ServingFlags.ResolvableDomain)
			if err != nil {
				logger.Errorf("error creating spoofing client %v", err)
				return
			}
			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s", domain), nil)
			if err != nil {
				logger.Errorf("error creating request %v", err)
				return
			}
			for {
				select {
				case <-done:
					return
				default:
					res, err := client.Do(req)
					if err != nil {
						logger.Errorf("error making request %v", err)
						return
					}
					if res.StatusCode != http.StatusOK {
						logger.Errorf("non 200 response %v", res.StatusCode)
						logger.Errorf("response headers: %v", res.Header)
						logger.Errorf("response body: %v", res.Body)
						return
					}
					totalRequests <- struct{}{}
					successfulRequests <- struct{}{}
				}
			}
		}()
	}

	want := 0
	go func() {
		for _ = range totalRequests {
			want++
		}
	}()
	got := 0
	go func() {
		for _ = range successfulRequests {
			got++
		}
	}()

	logger.Infof("Waiting for all requests to complete.")
	wg.Wait()
	close(totalRequests)
	close(successfulRequests)
	logger.Infof("All requests completed. Checking results.")
	if got != want {
		return fmt.Errorf("Error making requests for scale up. Got %v successful requests. Wanted %v.", got, want)
	}
	logger.Infof("Got %v successful responses.", want)
	return nil
}

func getAutoscalerConfigMap(clients *test.Clients) (*v1.ConfigMap, error) {
	return test.GetConfigMap(clients.KubeClient).Get("config-autoscaler", metav1.GetOptions{})
}

func setScaleToZeroThreshold(clients *test.Clients, threshold string, gracePeriod string) error {
	configMap, err := getAutoscalerConfigMap(clients)
	if err != nil {
		return err
	}
	configMap.Data["scale-to-zero-threshold"] = threshold
	configMap.Data["scale-to-zero-grace-period"] = gracePeriod
	_, err = test.GetConfigMap(clients.KubeClient).Update(configMap)
	return err
}

func setup(t *testing.T, logger *logging.BaseLogger) *test.Clients {
	clients := Setup(t)

	configMap, err := getAutoscalerConfigMap(clients)
	if err != nil {
		logger.Infof("Unable to retrieve the autoscale configMap. Assuming a ScaleToZero value of '5m'. %v", err)
		initialScaleToZeroThreshold = "5m"
		initialScaleToZeroGracePeriod = "2m"
	} else {
		initialScaleToZeroThreshold = configMap.Data["scale-to-zero-threshold"]
		initialScaleToZeroGracePeriod = configMap.Data["scale-to-zero-grace-period"]
	}

	err = setScaleToZeroThreshold(clients, "1m", "30s")
	if err != nil {
		t.Fatalf(`Unable to set ScaleToZeroThreshold to '1m'. This will
		          cause the test to time out. Failing fast instead. %v`, err)
	}

	return clients
}

func tearDown(clients *test.Clients, names test.ResourceNames, logger *logging.BaseLogger) {
	setScaleToZeroThreshold(clients, initialScaleToZeroThreshold, initialScaleToZeroGracePeriod)
	TearDown(clients, names, logger)
}

func TestAutoscaleUpDownUp(t *testing.T) {
	//add test case specific name to its own logger
	logger := logging.GetContextLogger("TestAutoscaleUpDownUp")
	clients := setup(t, logger)

	stopChan := DiagnoseMeEvery(15*time.Second, clients, logger)
	defer close(stopChan)

	imagePath := test.ImagePath("autoscale")

	logger.Infof("Creating a new Route and Configuration")
	names, err := CreateRouteAndConfig(clients, logger, imagePath, &test.Options{
		ContainerConcurrency: 10,
	})
	if err != nil {
		t.Fatalf("Failed to create Route and Configuration: %v", err)
	}
	test.CleanupOnInterrupt(func() { tearDown(clients, names, logger) }, logger)
	defer tearDown(clients, names, logger)

	logger.Infof(`When the Revision can have traffic routed to it,
	            the Route is marked as Ready.`)
	err = test.WaitForRouteState(
		clients.ServingClient,
		names.Route,
		test.IsRouteReady,
		"RouteIsReady")
	if err != nil {
		t.Fatalf(`The Route %s was not marked as Ready to serve traffic:
			 %v`, names.Route, err)
	}

	logger.Infof("Serves the expected data at the endpoint")
	config, err := clients.ServingClient.Configs.Get(names.Config, metav1.GetOptions{})
	if err != nil {
		t.Fatalf(`Configuration %s was not updated with the new
		         revision: %v`, names.Config, err)
	}
	deploymentName :=
		config.Status.LatestCreatedRevisionName + "-deployment"
	route, err := clients.ServingClient.Routes.Get(names.Route, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error fetching Route %s: %v", names.Route, err)
	}
	domain := route.Status.Domain

	_, err = pkgTest.WaitForEndpointState(
		clients.KubeClient,
		logger,
		domain,
		// Istio doesn't expose a status for us here: https://github.com/istio/istio/issues/6082
		// TODO(tcnghia): Remove this when https://github.com/istio/istio/issues/882 is fixed.
		pkgTest.Retrying(pkgTest.EventuallyMatchesBody(autoscaleExpectedOutput), http.StatusNotFound, http.StatusServiceUnavailable),
		"CheckingEndpointAfterUpdating",
		test.ServingFlags.ResolvableDomain)
	if err != nil {
		t.Fatalf(`The endpoint for Route %s at domain %s didn't serve
			 the expected text \"%v\": %v`,
			names.Route, domain, autoscaleExpectedOutput, err)
	}

	logger.Infof(`The autoscaler spins up additional replicas when traffic
		    increases.`)
	err = generateTraffic(clients, logger, 20, 20*time.Second, domain)
	if err != nil {
		logger.Fatalf("Error during initial scale up: %v", err)
	}
	logger.Info("Waiting for scale up")
	err = test.WaitForDeploymentState(
		clients.KubeClient,
		deploymentName,
		isDeploymentScaledUp(),
		"DeploymentIsScaledUp")
	if err != nil {
		logger.Fatalf(`Unable to observe the Deployment named %s scaling
			   up. %s`, deploymentName, err)
	}

	logger.Infof(`The autoscaler successfully scales down when devoid of
		    traffic.`)

	logger.Infof(`Manually setting ScaleToZeroThreshold to '1m' to facilitate
		    faster testing.`)

	logger.Infof("Waiting for scale to zero")
	err = test.WaitForDeploymentState(
		clients.KubeClient,
		deploymentName,
		isDeploymentScaledToZero(),
		"DeploymentScaledToZero")
	if err != nil {
		logger.Fatalf(`Unable to observe the Deployment named %s scaling
		           down. %s`, deploymentName, err)
	}

	// Account for the case where scaling up uses all available pods.
	logger.Infof("Wait for all pods to terminate.")

	err = test.WaitForPodListState(
		clients.KubeClient,
		func(p *v1.PodList) (bool, error) {
			for _, pod := range p.Items {
				if !strings.Contains(pod.Status.Reason, "Evicted") {
					return false, nil
				}
			}
			return true, nil
		},
		"WaitForAvailablePods")
	if err != nil {
		logger.Fatalf(`Waiting for Pod.List to have no non-Evicted pods: %v`, err)
	}

	logger.Infof("Scaled down.")
	logger.Infof("Sending traffic again to verify deployment scales back up")
	err = generateTraffic(clients, logger, 20, 30*time.Second, domain)
	if err != nil {
		t.Fatalf("Error during final scale up: %v", err)
	}
	err = test.WaitForDeploymentState(
		clients.KubeClient,
		deploymentName,
		isDeploymentScaledUp(),
		"DeploymentScaledUp")
	if err != nil {
		logger.Fatalf(`Unable to observe the Deployment named %s scaling
			   up. %s`, deploymentName, err)
	}
}
