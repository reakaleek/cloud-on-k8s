// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package stackmon

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/metadata"

	corev1 "k8s.io/api/core/v1"

	commonv1 "github.com/elastic/cloud-on-k8s/v3/pkg/apis/common/v1"
	esv1 "github.com/elastic/cloud-on-k8s/v3/pkg/apis/elasticsearch/v1"
	beatstackmon "github.com/elastic/cloud-on-k8s/v3/pkg/controller/beat/common/stackmon"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/defaults"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/stackmon"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/stackmon/monitoring"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/version"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/common/volume"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/elasticsearch/network"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/elasticsearch/securitycontext"
	"github.com/elastic/cloud-on-k8s/v3/pkg/controller/elasticsearch/user"
	esvolume "github.com/elastic/cloud-on-k8s/v3/pkg/controller/elasticsearch/volume"
	"github.com/elastic/cloud-on-k8s/v3/pkg/utils/k8s"
)

const (
	// cfgHashAnnotation is an annotation to store a hash of the Metricbeat and Filebeat configurations to rotate the Pods when changed.
	cfgHashAnnotation = "elasticsearch.k8s.elastic.co/monitoring-config-hash"
)

func Metricbeat(ctx context.Context, client k8s.Client, es esv1.Elasticsearch, meta metadata.Metadata) (stackmon.BeatSidecar, error) {
	username := user.MonitoringUserName
	password, err := user.GetMonitoringUserPassword(client, k8s.ExtractNamespacedName(&es))
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}

	v, err := version.Parse(es.Spec.Version)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}

	caVolume, err := stackmon.CAVolume(client, k8s.ExtractNamespacedName(&es), esv1.ESNamer, commonv1.EsMonitoringAssociationType, es.Spec.HTTP.TLS.Enabled())
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}

	input := stackmon.TemplateParams{
		URL:      fmt.Sprintf("%s://localhost:%d", es.Spec.HTTP.Protocol(), network.HTTPPort),
		Username: username,
		Password: password,
		IsSSL:    es.Spec.HTTP.TLS.Enabled(),
		CAVolume: caVolume,
	}

	cfg, err := stackmon.RenderTemplate(v, metricbeatConfigTemplate, input)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}

	metricbeat, err := stackmon.NewMetricBeatSidecar(ctx, client, &es, v, caVolume, cfg, meta)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}
	ver, err := version.Parse(es.Spec.Version)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}
	metricbeat.Container.SecurityContext = securitycontext.DefaultBeatSecurityContext(ver)
	return metricbeat, nil
}

func Filebeat(ctx context.Context, client k8s.Client, es esv1.Elasticsearch, meta metadata.Metadata) (stackmon.BeatSidecar, error) {
	fileBeat, err := stackmon.NewFileBeatSidecar(ctx, client, &es, es.Spec.Version, filebeatConfig, nil, meta)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}
	ver, err := version.Parse(es.Spec.Version)
	if err != nil {
		return stackmon.BeatSidecar{}, err
	}
	fileBeat.Container.SecurityContext = securitycontext.DefaultBeatSecurityContext(ver)
	return fileBeat, nil
}

// WithMonitoring updates the Elasticsearch Pod template builder to deploy Metricbeat and Filebeat in sidecar containers
// in the Elasticsearch pod and injects the volumes for the beat configurations and the ES CA certificates.
func WithMonitoring(ctx context.Context, client k8s.Client, builder *defaults.PodTemplateBuilder, es esv1.Elasticsearch, meta metadata.Metadata) (*defaults.PodTemplateBuilder, error) {
	isMonitoringReconcilable, err := monitoring.IsReconcilable(&es)
	if err != nil {
		return nil, err
	}
	if !isMonitoringReconcilable {
		return builder, nil
	}

	configHash := fnv.New32a()
	volumes := make([]corev1.Volume, 0)

	if monitoring.IsMetricsDefined(&es) {
		b, err := Metricbeat(ctx, client, es, meta)
		if err != nil {
			return nil, err
		}

		// Add metricbeat logs volume
		metricbeatLogsVolume := volume.NewEmptyDirVolume(beatstackmon.MetricbeatLogsVolumeName, beatstackmon.MetricbeatLogsVolumeMountPath)
		volumes = append(volumes, metricbeatLogsVolume.Volume())
		b.Container.VolumeMounts = append(b.Container.VolumeMounts, metricbeatLogsVolume.VolumeMount())

		volumes = append(volumes, b.Volumes...)
		builder.WithContainers(b.Container)
		configHash.Write(b.ConfigHash.Sum(nil))
	}

	if monitoring.IsLogsDefined(&es) {
		// enable Stack logging to write Elasticsearch logs to disk
		builder.WithEnv(fileLogStyleEnvVar())

		b, err := Filebeat(ctx, client, es, meta)
		if err != nil {
			return nil, err
		}

		// Add filebeat logs volume
		filebeatLogsVolume := volume.NewEmptyDirVolume(beatstackmon.FilebeatLogsVolumeName, beatstackmon.FilebeatLogsVolumeMountPath)
		volumes = append(volumes, filebeatLogsVolume.Volume())
		b.Container.VolumeMounts = append(b.Container.VolumeMounts, filebeatLogsVolume.VolumeMount())

		volumes = append(volumes, b.Volumes...)
		filebeat := b.Container

		// share the ES logs volume into the Filebeat container
		filebeat.VolumeMounts = append(filebeat.VolumeMounts, esvolume.DefaultLogsVolumeMount)

		builder.WithContainers(filebeat)
		configHash.Write(b.ConfigHash.Sum(nil))
	}

	// add the config hash annotation to ensure pod rotation when an ES password or a CA are rotated
	builder.WithAnnotations(map[string]string{cfgHashAnnotation: fmt.Sprint(configHash.Sum32())})
	// inject all volumes
	builder.WithVolumes(volumes...)

	return builder, nil
}
