/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	runnerLabelKey  = "nvs-harness/runner"
	socketHostPath  = "/var/run/nvsentinel"
	resultsHostPath = "/var/log/nvs-harness"
)

// jobSpec describes an in-cluster harnessctl Job (inject or reconcile).
type jobSpec struct {
	name        string
	args        []string
	env         []corev1.EnvVar
	mountSocket bool   // inject needs the connector UDS
	mongoSecret string // reconcile mounts this TLS secret at /etc/mongo-certs (empty = none)
}

// runJob creates the Job (pinned to the runner node), waits for completion, and
// returns the last pod's logs. Deletes any prior Job of the same name first.
func (c *clients) runJob(ctx context.Context, cfg Config, js jobSpec, timeout time.Duration) (string, error) {
	ns := cfg.NVSNamespace
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, js.name, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationForeground)})
	// Wait for prior deletion to settle.
	_ = c.waitJobGone(ctx, ns, js.name, 60*time.Second)

	vols := []corev1.Volume{{
		Name:         "results",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: resultsHostPath, Type: ptr(corev1.HostPathDirectoryOrCreate)}},
	}}
	mounts := []corev1.VolumeMount{{Name: "results", MountPath: "/results"}}
	if js.mountSocket {
		vols = append(vols, corev1.Volume{
			Name:         "var-run-vol",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: socketHostPath, Type: ptr(corev1.HostPathDirectoryOrCreate)}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "var-run-vol", MountPath: "/var/run"})
	}
	if js.mongoSecret != "" {
		vols = append(vols, corev1.Volume{
			Name:         "mongo-certs",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: js.mongoSecret}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "mongo-certs", MountPath: "/etc/mongo-certs", ReadOnly: true})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: js.name, Namespace: ns, Labels: map[string]string{"app": js.name}},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			TTLSecondsAfterFinished: ptr(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": js.name}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  map[string]string{runnerLabelKey: "true"},
					Containers: []corev1.Container{{
						Name:            js.name,
						Image:           cfg.HarnessImage,
						// IfNotPresent (not Always) so kind/air-gapped runs can use a
						// locally sideloaded image; real registries still pull on miss.
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            js.args,
						Env:             js.env,
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{RunAsUser: ptr(int64(0))},
					}},
					Volumes: vols,
				},
			},
		},
	}
	if _, err := c.kube.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create job %s: %w", js.name, err)
	}
	infof("job %s created; waiting up to %s", js.name, timeout)

	if err := c.waitJob(ctx, ns, js.name, timeout); err != nil {
		logs, _ := c.jobPodLogs(ctx, ns, js.name)
		return logs, err
	}
	return c.jobPodLogs(ctx, ns, js.name)
}

func (c *clients) waitJob(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		job, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
					return nil
				}
				if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
					return fmt.Errorf("job %s failed: %s", name, cond.Message)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s did not complete within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (c *clients) waitJobGone(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s still present after %s", name, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// jobPodLogs returns the logs of the (most recent) pod belonging to the Job.
func (c *clients) jobPodLogs(ctx context.Context, ns, jobName string) (string, error) {
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + jobName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods for job %s", jobName)
	}
	pod := pods.Items[len(pods.Items)-1]
	rc, err := c.kube.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func ptr[T any](v T) *T { return &v }

// singleEnv builds a one-entry env slice for a Job container.
func singleEnv(name, value string) []corev1.EnvVar {
	return []corev1.EnvVar{{Name: name, Value: value}}
}
