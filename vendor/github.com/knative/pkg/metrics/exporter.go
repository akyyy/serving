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

package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"contrib.go.opencensus.io/exporter/stackdriver/monitoredresource"
	"github.com/knative/pkg/metrics/metricskey"
	"go.opencensus.io/exporter/prometheus"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/zap"
	// monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
)

var (
	curMetricsExporter view.Exporter
	curMetricsConfig   *metricsConfig
	curPromSrv         *http.Server
	//	curGcpMetadata     *gcpMetadata
	getMonitoredResourceFunc func(v *view.View, tags []tag.Tag) ([]tag.Tag, monitoredresource.Interface)
	metricsMux               sync.Mutex
)

// newMetricsExporter gets a metrics exporter based on the config.
func newMetricsExporter(config *metricsConfig, logger *zap.SugaredLogger) error {
	// If there is a Prometheus Exporter server running, stop it.
	resetCurPromSrv()
	ce := getCurMetricsExporter()
	if ce != nil {
		// UnregisterExporter is idempotent and it can be called multiple times for the same exporter
		// without side effects.
		view.UnregisterExporter(ce)
	}
	var err error
	var e view.Exporter
	switch config.backendDestination {
	case Stackdriver:
		e, err = newStackdriverExporter(config, logger)
		// Set getMonitoredResourceFunc
		setMonitoredResourceFunc(config)
	case Prometheus:
		e, err = newPrometheusExporter(config, logger)
	default:
		err = fmt.Errorf("Unsupported metrics backend %v", config.backendDestination)
	}
	if err != nil {
		return err
	}
	existingConfig := getCurMetricsConfig()
	setCurMetricsExporterAndConfig(e, config)
	logger.Infof("Successfully updated the metrics exporter; old config: %v; new config %v", existingConfig, config)
	return nil
}

func getKnativeRevisionMonitoredResource(gm *gcpMetadata) func(v *view.View, tags []tag.Tag) ([]tag.Tag, monitoredresource.Interface) {
	// var newTags []tag.Tag
	// for _, t := range tags {
	// 	v := vb.ReadValue()	// 	if v != nil {
	// 		newTags = append(newTags, tag.Tag{Key: t, Value: string(v)})
	// 	}
	// }

	return func(v *view.View, tags []tag.Tag) ([]tag.Tag, monitoredresource.Interface) {
		// TODO: After knative_revision is onboarded, replace resource type gke_container.
		gkeContainer := &monitoredresource.GKEContainer{
			ProjectID:     gm.project,
			ClusterName:   gm.cluster,
			Zone:          gm.location,
			NamespaceID:   "testNamespace1", // use this field for revision namespace
			ContainerName: "container1",     // use this field for service name
			InstanceID:    "instance1",      // use this field for configuration name
			PodID:         "pod1",           // use this field for revision name
		}

		// TODO: After knative_revision is onbaroded, use resource type knative_revision
		// as follows
		// kr := &KnativeRevision{
		// 	Project:           gm.project,
		// 	Location:          gm.location,
		// 	ClusterName:       gm.cluster,
		// 	NamespaceName:     "testNamespace",
		// 	ServiceName:       "testService",
		// 	ConfigurationName: "testConfig",
		// 	RevisionName:      "testRev",
		// }

		return tags, gkeContainer
	}
}

func getGlobalMonitoredResource() func(v *view.View, tags []tag.Tag) ([]tag.Tag, monitoredresource.Interface) {
	return func(v *view.View, tags []tag.Tag) ([]tag.Tag, monitoredresource.Interface) {
		return tags, &Global{}
	}
}

func newStackdriverExporter(config *metricsConfig, logger *zap.SugaredLogger) (view.Exporter, error) {
	e, err := stackdriver.NewExporter(stackdriver.Options{
		ProjectID:    config.stackdriverProjectID,
		MetricPrefix: config.domain + "/" + config.component,
		// Resource: &monitoredrespb.MonitoredResource{
		// 	Type: "global",
		// },

		// MonitoredResource:       &gkeContainer,

		GetMonitoredResource:    getMonitoredResourceFunc,
		DefaultMonitoringLabels: &stackdriver.Labels{},
	})
	if err != nil {
		logger.Error("Failed to create the Stackdriver exporter.", zap.Error(err))
		return nil, err
	}
	logger.Infof("Created Opencensus Stackdriver exporter with config %v", config)
	return e, nil
}

func newPrometheusExporter(config *metricsConfig, logger *zap.SugaredLogger) (view.Exporter, error) {
	e, err := prometheus.NewExporter(prometheus.Options{Namespace: config.component})
	if err != nil {
		logger.Error("Failed to create the Prometheus exporter.", zap.Error(err))
		return nil, err
	}
	logger.Infof("Created Opencensus Prometheus exporter with config: %v. Start the server for Prometheus exporter.", config)
	// Start the server for Prometheus scraping
	go func() {
		srv := startNewPromSrv(e)
		srv.ListenAndServe()
	}()
	return e, nil
}

func getCurPromSrv() *http.Server {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	return curPromSrv
}

func resetCurPromSrv() {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	if curPromSrv != nil {
		curPromSrv.Close()
		curPromSrv = nil
	}
}

func setMonitoredResourceFunc(config *metricsConfig) {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	if getMonitoredResourceFunc == nil {
		gm := retrieveGCPMetadata()
		fmt.Println("metrics prefix", config.domain+"/"+config.component)
		if _, ok := metricskey.KnativeRevisionMetricsPrefixes[config.domain+"/"+config.component]; ok {
			fmt.Println("path 1")
			getMonitoredResourceFunc = getKnativeRevisionMonitoredResource(gm)
		} else {
			fmt.Println("path 2")
			getMonitoredResourceFunc = getGlobalMonitoredResource()
		}
	}
}

func startNewPromSrv(e *prometheus.Exporter) *http.Server {
	sm := http.NewServeMux()
	sm.Handle("/metrics", e)
	metricsMux.Lock()
	defer metricsMux.Unlock()
	if curPromSrv != nil {
		curPromSrv.Close()
	}
	curPromSrv = &http.Server{
		Addr:    ":9090",
		Handler: sm,
	}
	return curPromSrv
}

func getCurMetricsExporter() view.Exporter {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	return curMetricsExporter
}

func setCurMetricsExporterAndConfig(e view.Exporter, c *metricsConfig) {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	view.RegisterExporter(e)
	view.SetReportingPeriod(60 * time.Second)
	curMetricsExporter = e
	curMetricsConfig = c
}

func getCurMetricsConfig() *metricsConfig {
	metricsMux.Lock()
	defer metricsMux.Unlock()
	return curMetricsConfig
}
