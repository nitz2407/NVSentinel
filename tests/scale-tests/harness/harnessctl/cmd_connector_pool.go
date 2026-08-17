/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	connectorPoolName  = "nvs-harness-connector-pool"
	connectorPoolLabel = "nvs-harness/pool"
	// poolSocketRoot is the node hostPath under which each pool connector gets a
	// private per-pod socket dir (via subPathExpr=$(POD_NAME)), so packed
	// connectors on one node don't collide and the node's injector can reach
	// every connector's socket.
	poolSocketRoot         = "/var/run/nvs-harness-pool"
	poolEmulatedAnnotation = "nvs-harness/emulated-nodes"

	// Persistent per-node injector: slim multi-arch harness-inject image
	// (node kubelet pulls linux/amd64 or linux/arm64 automatically).
	poolInjectorDaemonSet = "nvs-harness-pool-injector"
	poolInjectorLabel     = "nvs-harness/pool-injector"
	// poolNodeLabel marks the real nodes hosting pool connectors, so the injector
	// DaemonSet lands exactly there (one injector per connector node) instead of
	// on every non-kwok node in a shared cluster.
	poolNodeLabel = "nvs-harness/pool-node"
)

// poolSizing is the P0.5 simulation-fidelity record: how faithfully the real
// connector pool (packed onto real nodes up to the pod-IP ceiling) reproduces a
// per-node DaemonSet across the emulated fleet, and the event-rate multiplier
// used to represent the nodes that got no dedicated connector.
type poolSizing struct {
	EmulatedNodes     int     `json:"emulated_nodes"`
	RealNodes         int     `json:"real_nodes"`
	PerNodePodLimit   int     `json:"per_node_pod_limit"`
	PodCeiling        int     `json:"pod_ceiling"`         // realNodes * perNodePodLimit
	RealConnectors    int     `json:"real_connectors"`     // StatefulSet replicas
	PerNodeDensity    float64 `json:"per_node_density"`    // realConnectors / realNodes
	NodesPerConnector int     `json:"nodes_per_connector"` // ceil(emulated / realConnectors)
	RateMultiplier    int     `json:"rate_multiplier"`     // == NodesPerConnector
	RealToEmulated    float64 `json:"real_to_emulated_ratio"`
	PodCeilingReached bool    `json:"pod_ceiling_reached"`
}

// computePoolSizing derives the pool geometry. One real connector per emulated
// node is ideal; when that exceeds the pod ceiling (realNodes * per-node limit)
// the pool is capped there and the shortfall is compensated by raising each
// connector's event rate (rateMultiplier = nodes each connector represents). The
// connector count is therefore always auto-sized to min(emulated, ceiling); the
// only knob is the per-node density cap.
func computePoolSizing(emulatedNodes, realNodes, perNodeLimit int) poolSizing {
	if realNodes < 1 {
		realNodes = 1
	}
	if perNodeLimit < 1 {
		perNodeLimit = 1
	}
	if emulatedNodes < 1 {
		emulatedNodes = 1
	}
	ceiling := realNodes * perNodeLimit

	replicas := emulatedNodes
	if replicas > ceiling {
		replicas = ceiling
	}
	if replicas < 1 {
		replicas = 1
	}

	nodesPerConn := int(math.Ceil(float64(emulatedNodes) / float64(replicas)))
	if nodesPerConn < 1 {
		nodesPerConn = 1
	}
	return poolSizing{
		EmulatedNodes:     emulatedNodes,
		RealNodes:         realNodes,
		PerNodePodLimit:   perNodeLimit,
		PodCeiling:        ceiling,
		RealConnectors:    replicas,
		PerNodeDensity:    float64(replicas) / float64(realNodes),
		NodesPerConnector: nodesPerConn,
		RateMultiplier:    nodesPerConn,
		RealToEmulated:    float64(replicas) / float64(emulatedNodes),
		PodCeilingReached: replicas >= ceiling && emulatedNodes > ceiling,
	}
}

// runConnectorPool is the P0.5 "stage" command: it deploys the platform-connector
// pods (packed onto real nodes up to the pod-IP ceiling) plus one resident
// injector per connector node, sized to represent the live KWOK fleet. It does
// NOT inject events — firing events is P0.3's job (the `inject` + `reconcile`
// commands, which drive the resident injectors this command deploys).
// runPoolCreate stages the connector pool (default P0.5 action).
func runPoolCreate(ctx context.Context, args []string) error { return runConnectorPool(ctx, args) }

// runPoolTeardown deletes the harness-owned connector pool + resident injectors
// only (nvs-harness-*); never the live platform-connectors DaemonSet.
func runPoolTeardown(ctx context.Context, args []string) error {
	return runConnectorPool(ctx, append([]string{"-teardown"}, args...))
}

// runPoolStartupBurst runs the client-go burst APF-saturation experiment.
func runPoolStartupBurst(ctx context.Context, args []string) error {
	return runConnectorPool(ctx, append([]string{"-startup-burst"}, args...))
}

// runPoolConnectionSweep runs the replica/connection sweep experiment.
func runPoolConnectionSweep(ctx context.Context, args []string) error {
	return runConnectorPool(ctx, append([]string{"-connection-sweep"}, args...))
}

func runConnectorPool(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pool", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindResultsFlag(fs, &cfg)
	bindPromFlags(fs, &cfg)
	fs.StringVar(&cfg.ConnectorDaemonSet, "connector-daemonset", cfg.ConnectorDaemonSet, "read-only: DaemonSet whose pod template the pool clones (never deleted/scaled by harnessctl)")
	bindMongoTLSSecretFlag(fs, &cfg)
	bindInjectorFlags(fs, &cfg)
	perNodeLimit := fs.Int("per-node-pod-limit", cfg.ConnectorPoolPerNodeLimit, "connector pods/node density cap; connector count = min(live KWOK nodes, realNodes*this)")
	teardown := fs.Bool("teardown", false, "delete harness-owned pool (nvs-harness-*) + injectors only; never touches platform-connectors")
	startupBurst := fs.Bool("startup-burst", false, "experiment: recreate the pool with -replicas connectors started simultaneously across -burst-steps client-go burst values; measure APF saturation at startup")
	burstSteps := fs.String("burst-steps", "10,15,40", "startup-burst: comma-separated client-go burst values to sweep")
	burstReplicas := fs.Int("replicas", 0, "startup-burst: fixed connector replica count (0 => current live pool size)")
	connSweep := fs.Bool("connection-sweep", false, "experiment: create pool → scale across -replica-steps (Mongo conns/CPU/mem) → teardown")
	replicaSteps := fs.String("replica-steps", "5,10,50,100,200", "connection-sweep: comma-separated replica counts to sweep (create starts at the first value)")
	settle := fs.Int("settle-seconds", 30, "connection-sweep: seconds to wait for connections to stabilize before measuring each step")
	window := fs.String("window", cfg.MetricsWindow, "PromQL rate window for sweep measurements")
	_ = fs.Parse(args)

	cfg.ConnectorPoolPerNodeLimit = *perNodeLimit

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	if *teardown {
		stepf("connector-pool: teardown")
		return c.teardownConnectorPool(ctx, cfg)
	}

	if *startupBurst {
		steps := parseIntCSV(*burstSteps)
		if len(steps) == 0 {
			return fmt.Errorf("-burst-steps parsed to nothing; give e.g. -burst-steps 10,15,40")
		}
		stepf("connector-pool: startup-burst sweep %v (window %s)", steps, *window)
		return c.startupBurstSweep(ctx, cfg, *burstReplicas, steps, *window)
	}

	if *connSweep {
		steps := parseIntCSV(*replicaSteps)
		if len(steps) == 0 {
			return fmt.Errorf("-replica-steps parsed to nothing; give e.g. -replica-steps 5,10,50")
		}
		stepf("connector-pool: connection sweep %v (settle %ds, window %s)", steps, *settle, *window)
		return c.connectionSweep(ctx, cfg, steps, *settle, *window)
	}

	// The emulated fleet the pool must represent IS the live KWOK fleet — derive
	// it instead of taking a flag, so it always matches what `scale-nodes` created.
	emulated := c.countKwokNodesOrZero(ctx)
	if emulated <= 0 {
		return fmt.Errorf("no live KWOK nodes found: run `scale-nodes -count N` first so the pool can size to the emulated fleet")
	}
	rs := newResultSet(cfg.ResultsDir)
	res := c.checkConnectorPool(ctx, cfg, emulated)
	rs.add(res)
	_ = rs.write()
	if !res.passed() {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// schedulableRealNodes counts nodes a connector pod can land on: real (non-kwok)
// and Ready. Connectors tolerate all taints (cloned from the DaemonSet).
func (c *clients) schedulableRealNodes(ctx context.Context) (int, error) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range list.Items {
		if node.Labels["type"] == "kwok" {
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				n++
				break
			}
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no schedulable real (non-kwok) nodes found")
	}
	return n, nil
}

// buildConnectorPool clones the live platform-connectors DaemonSet pod template
// (Get only — the template DaemonSet is never modified or deleted) into a
// StatefulSet packed onto real nodes. The socket-backing volume becomes a
// node hostPath rooted at poolSocketRoot, mounted per-pod via
// subPathExpr=$(POD_NAME) (kubelet expands it — no shell needed, works with the
// distroless connector) so packed connectors on one node land in private subdirs
// and the node's injector can reach every socket; all other hostPath volumes
// become per-pod emptyDirs. Reusing the real container/image/config/mTLS makes
// the summed write plane (datastore connections, K8s clients, API writes)
// faithful to production. Injection is driven externally, on demand, so no
// injector runs inside the connector pods.
func (c *clients) buildConnectorPool(ctx context.Context, cfg Config, sizing poolSizing) (*appsv1.StatefulSet, *corev1.Service, error) {
	ds, err := c.kube.AppsV1().DaemonSets(cfg.NVSNamespace).Get(ctx, cfg.ConnectorDaemonSet, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("get DaemonSet %s (connector template): %w", cfg.ConnectorDaemonSet, err)
	}
	podSpec := *ds.Spec.Template.Spec.DeepCopy()

	if len(podSpec.Containers) == 0 {
		return nil, nil, fmt.Errorf("connector DaemonSet has no containers")
	}
	_, socketMount := connectorSocket(podSpec.Containers)

	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].HostPath == nil {
			continue
		}
		if podSpec.Volumes[i].Name == socketMount.Name {
			podSpec.Volumes[i].HostPath = &corev1.HostPathVolumeSource{
				Path: poolSocketRoot, Type: ptr(corev1.HostPathDirectoryOrCreate),
			}
			continue
		}
		podSpec.Volumes[i].HostPath = nil
		podSpec.Volumes[i].EmptyDir = &corev1.EmptyDirVolumeSource{}
	}
	setPerPodSocketSubPath(&podSpec, socketMount.Name)

	// Pin the pool to real nodes; keep the DaemonSet's broad tolerations so it can
	// still land on the (tainted) GPU nodes.
	podSpec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "type", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"kwok"},
			}},
		}}},
	}}

	labels := map[string]string{"app": connectorPoolName, connectorPoolLabel: connectorPoolName}
	replicas := int32(sizing.RealConnectors)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: connectorPoolName, Namespace: cfg.NVSNamespace, Labels: labels,
			Annotations: map[string]string{
				poolEmulatedAnnotation:            strconv.Itoa(sizing.EmulatedNodes),
				"nvs-harness/nodes-per-connector": strconv.Itoa(sizing.NodesPerConnector),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            &replicas,
			ServiceName:         connectorPoolName,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{connectorPoolLabel: connectorPoolName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: connectorPoolName, Namespace: cfg.NVSNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{connectorPoolLabel: connectorPoolName},
			Ports:     []corev1.ServicePort{{Name: "metrics", Port: 2112}},
		},
	}
	return sts, svc, nil
}

// connectorSocket resolves the connector's Unix socket path (from its --socket
// arg, default /var/run/nvsentinel.sock) and the volume mount that backs the
// socket's directory, so the pool can re-root that volume on the node hostPath.
func connectorSocket(containers []corev1.Container) (string, corev1.VolumeMount) {
	socketPath := "/var/run/nvsentinel.sock"
	ci := 0
	for idx := range containers {
		for _, a := range containers[idx].Args {
			if strings.HasPrefix(a, "--socket=") {
				socketPath = strings.TrimPrefix(a, "--socket=")
				ci = idx
			}
		}
	}
	socketDir := path.Dir(socketPath)
	var best corev1.VolumeMount
	for _, m := range containers[ci].VolumeMounts {
		mp := strings.TrimRight(m.MountPath, "/")
		if mp == socketDir || strings.HasPrefix(socketPath, mp+"/") {
			// Prefer the deepest (most specific) mount covering the socket.
			if len(m.MountPath) > len(best.MountPath) {
				best = m
			}
		}
	}
	if best.Name == "" {
		best = corev1.VolumeMount{Name: "var-run-vol", MountPath: socketDir}
	}
	return socketPath, best
}

// setPerPodSocketSubPath makes each connector land its socket in a private
// per-pod subdir of the poolSocketRoot hostPath: it sets
// subPathExpr=$(POD_NAME) on every mount of the socket volume and injects a
// POD_NAME (downward API) env into the owning container so kubelet can expand
// it. Result on the node: poolSocketRoot/<pod-name>/nvsentinel.sock.
func setPerPodSocketSubPath(podSpec *corev1.PodSpec, socketVol string) {
	const podNameEnv = "POD_NAME"
	for ci := range podSpec.Containers {
		touched := false
		for mi := range podSpec.Containers[ci].VolumeMounts {
			if podSpec.Containers[ci].VolumeMounts[mi].Name == socketVol {
				podSpec.Containers[ci].VolumeMounts[mi].SubPathExpr = "$(" + podNameEnv + ")"
				touched = true
			}
		}
		if !touched {
			continue
		}
		hasEnv := false
		for _, e := range podSpec.Containers[ci].Env {
			if e.Name == podNameEnv {
				hasEnv = true
				break
			}
		}
		if !hasEnv {
			podSpec.Containers[ci].Env = append(podSpec.Containers[ci].Env, corev1.EnvVar{
				Name:      podNameEnv,
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
			})
		}
	}
}

// applyConnectorPool (re)creates the pool StatefulSet + headless Service.
//
// A startup-burst writes the override ConfigMap (poolConfigConfigMap) immediately
// before calling this, and retargets the STS volume at it. Teardown used to delete
// that ConfigMap, so the recreated pods stuck in Init with
// "configmap nvs-harness-connector-pool-config not found". Preserve any existing
// override ConfigMap across the recreate.
func (c *clients) applyConnectorPool(ctx context.Context, cfg Config, sts *appsv1.StatefulSet, svc *corev1.Service) error {
	var savedCM *corev1.ConfigMap
	if cm, err := c.kube.CoreV1().ConfigMaps(cfg.NVSNamespace).Get(ctx, poolConfigConfigMap, metav1.GetOptions{}); err == nil {
		savedCM = cm.DeepCopy()
		savedCM.ResourceVersion = ""
		savedCM.UID = ""
		savedCM.CreationTimestamp = metav1.Time{}
	}

	_ = c.teardownConnectorPool(ctx, cfg)
	// Wait for a prior StatefulSet to fully clear so the recreate doesn't conflict.
	c.waitStatefulSetGone(ctx, cfg.NVSNamespace, connectorPoolName, 90*time.Second)

	if savedCM != nil {
		if _, err := c.kube.CoreV1().ConfigMaps(cfg.NVSNamespace).Create(ctx, savedCM, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("restore pool config ConfigMap: %w", err)
		}
	}

	if _, err := c.kube.CoreV1().Services(cfg.NVSNamespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create headless service: %w", err)
	}
	if _, err := c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Create(ctx, sts, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create statefulset: %w", err)
	}
	infof("connector pool applied: %s (replicas=%d)", connectorPoolName, *sts.Spec.Replicas)
	return nil
}

// teardownConnectorPool removes only harness-owned pool objects (names under
// harnessOwnedPrefix). The live platform-connectors DaemonSet and its pods are
// never deleted, scaled, or patched here — used by pool teardown, stack
// cleanup --pool, applyConnectorPool recreate, and connection-sweep.
func (c *clients) teardownConnectorPool(ctx context.Context, cfg Config) error {
	ns := cfg.NVSNamespace
	pol := metav1.DeletionPropagation(metav1.DeletePropagationForeground)
	// Tear down the co-located injector DaemonSet first, then clear the node
	// labels that pinned it, so injectors don't outlive the pool.
	if err := c.deleteHarnessDaemonSet(ctx, ns, poolInjectorDaemonSet, cfg.ConnectorDaemonSet, pol); err != nil {
		return err
	}
	c.unlabelPoolNodes(ctx)
	if err := c.deleteHarnessStatefulSet(ctx, ns, connectorPoolName, pol); err != nil {
		return err
	}
	if err := c.deleteHarnessService(ctx, ns, connectorPoolName); err != nil {
		return err
	}
	// Drop the burst-override ConfigMap left by a startup-burst experiment (if any).
	if err := c.deleteHarnessConfigMap(ctx, ns, poolConfigConfigMap); err != nil {
		return err
	}
	infof("connector pool torn down (harness-owned statefulset + service + injector only; platform-connectors untouched)")
	return nil
}

// currentPoolReplicas returns the deployed pool's desired replica count, or -1 if
// no pool StatefulSet exists.
func (c *clients) currentPoolReplicas(ctx context.Context, cfg Config) int {
	sts, err := c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Get(ctx, connectorPoolName, metav1.GetOptions{})
	if err != nil {
		return -1
	}
	if sts.Spec.Replicas == nil {
		return 0
	}
	return int(*sts.Spec.Replicas)
}

// unlabelPoolNodes strips poolNodeLabel from every node that carried it, so the
// cluster is left clean after teardown.
func (c *clients) unlabelPoolNodes(ctx context.Context) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: poolNodeLabel + "=true"})
	if err != nil {
		return
	}
	for i := range list.Items {
		_ = c.unlabelNode(ctx, list.Items[i].Name, poolNodeLabel)
	}
}

func (c *clients) waitDaemonSetGone(ctx context.Context, ns, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.kube.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// waitDaemonSetReady blocks until every scheduled injector pod is Ready (or
// timeout), returning the ready count and whether it matched the desired count.
func (c *clients) waitDaemonSetReady(ctx context.Context, ns, name string, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last := -1
	for {
		ds, err := c.kube.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			desired := int(ds.Status.DesiredNumberScheduled)
			ready := int(ds.Status.NumberReady)
			if ready != last {
				infof("injector DaemonSet: %d/%d ready", ready, desired)
				last = ready
			}
			if desired > 0 && ready >= desired {
				return ready, true
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-tick.C:
		}
	}
}

func (c *clients) waitStatefulSetGone(ctx context.Context, ns, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *clients) waitStatefulSetReady(ctx context.Context, ns, name string, want int, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last := -1
	for {
		sts, err := c.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			ready := int(sts.Status.ReadyReplicas)
			if ready != last {
				infof("connector pool: %d/%d ready", ready, want)
				last = ready
			}
			if ready >= want {
				return ready, true
			}
		}
		if time.Now().After(deadline) {
			r := 0
			if sts != nil {
				r = int(sts.Status.ReadyReplicas)
			}
			return r, false
		}
		select {
		case <-ctx.Done():
			return last, false
		case <-tick.C:
		}
	}
}

// checkConnectorPool (P0.5) deploys the packed connector pool on real/GPU nodes
// and co-locates a persistent injector on each connector node. Each connector
// exposes its socket on a private per-pod node path
// (poolSocketRoot/<pod>/nvsentinel.sock), so events are NOT sent at deploy time;
// injection is triggered later, on demand, by `harnessctl inject` (which fires
// every resident injector) — giving the operator control over WHEN health events
// flow to the connectors.
func (c *clients) checkConnectorPool(ctx context.Context, cfg Config, emulated int) CheckResult {
	started := time.Now()
	stepf("P0.5: connector pool on real/GPU nodes (emulated fleet=%d)", emulated)
	res := CheckResult{ID: "P0.5", Name: "connector pool", Started: started, Metrics: map[string]any{}}

	realNodes, err := c.schedulableRealNodes(ctx)
	if err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", err.Error(), time.Now()
		return res
	}
	sizing := computePoolSizing(emulated, realNodes, cfg.ConnectorPoolPerNodeLimit)
	infof("pool sizing: real_nodes=%d ceiling=%d replicas=%d density=%.1f/node nodes/connector=%d",
		sizing.RealNodes, sizing.PodCeiling, sizing.RealConnectors, sizing.PerNodeDensity, sizing.NodesPerConnector)
	res.Metrics["sizing"] = sizing
	writeArtifact(cfg.ResultsDir, "p0.5-connector-pool.json", map[string]any{"sizing": sizing})

	sts, svc, err := c.buildConnectorPool(ctx, cfg, sizing)
	if err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", "build pool: "+err.Error(), time.Now()
		return res
	}
	if err := c.applyConnectorPool(ctx, cfg, sts, svc); err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", "deploy pool: "+err.Error(), time.Now()
		return res
	}
	readyTO := time.Duration(sizing.RealConnectors*3+120) * time.Second
	ready, ok := c.waitStatefulSetReady(ctx, cfg.NVSNamespace, connectorPoolName, sizing.RealConnectors, readyTO)
	res.Metrics["connectors_ready"] = ready
	if !ok {
		res.Finished = time.Now()
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("only %d/%d connector pods Ready within %s", ready, sizing.RealConnectors, readyTO)
		return res
	}

	// Co-deploy a persistent injector on every connector-hosting node: label the
	// nodes the pool packed onto, then run a DaemonSet pinned to those nodes so an
	// injector sits inside each node alongside its connectors, always ready to fire.
	injNodes, err := c.deployPoolInjectors(ctx, cfg)
	if err != nil {
		res.Finished = time.Now()
		res.Verdict = "FAIL"
		res.Message = "deploy injectors: " + err.Error()
		return res
	}
	res.Metrics["injector_nodes"] = injNodes
	injTO := 120 * time.Second
	injReady, injOK := c.waitDaemonSetReady(ctx, cfg.NVSNamespace, poolInjectorDaemonSet, injTO)
	res.Metrics["injectors_ready"] = injReady
	res.Finished = time.Now()
	if !injOK {
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("connectors ready but only %d/%d node injectors Ready within %s", injReady, injNodes, injTO)
		return res
	}

	res.Verdict = "PASS"
	res.Message = fmt.Sprintf("staged %d connectors (%.1f/node) + %d persistent node injectors (image %s); sockets under %s/<pod>; fire events with the P0.3 inject/reconcile flow",
		ready, sizing.PerNodeDensity, injReady, cfg.injectorPodImage(), poolSocketRoot)
	return res
}

// deployPoolInjectors labels every node currently hosting a pool connector with
// poolNodeLabel and (re)creates the injector DaemonSet pinned to those nodes, so
// exactly one persistent injector pod runs inside each connector node. Returns
// the number of connector nodes targeted.
func (c *clients) deployPoolInjectors(ctx context.Context, cfg Config) (int, error) {
	pods, err := c.kube.CoreV1().Pods(cfg.NVSNamespace).List(ctx, metav1.ListOptions{LabelSelector: connectorPoolLabel + "=" + connectorPoolName})
	if err != nil {
		return 0, err
	}
	nodes := map[string]struct{}{}
	for i := range pods.Items {
		if n := pods.Items[i].Spec.NodeName; n != "" {
			nodes[n] = struct{}{}
		}
	}
	if len(nodes) == 0 {
		return 0, fmt.Errorf("no scheduled pool pods to co-locate injectors with")
	}
	for n := range nodes {
		if err := c.labelNode(ctx, n, poolNodeLabel, "true"); err != nil {
			return 0, fmt.Errorf("label node %s: %w", n, err)
		}
	}
	ds := buildInjectorDaemonSet(cfg)
	pol := metav1.DeletionPropagation(metav1.DeletePropagationForeground)
	if err := c.deleteHarnessDaemonSet(ctx, cfg.NVSNamespace, poolInjectorDaemonSet, cfg.ConnectorDaemonSet, pol); err != nil {
		return 0, err
	}
	c.waitDaemonSetGone(ctx, cfg.NVSNamespace, poolInjectorDaemonSet, 60*time.Second)
	if _, err := c.kube.AppsV1().DaemonSets(cfg.NVSNamespace).Create(ctx, ds, metav1.CreateOptions{}); err != nil {
		return 0, fmt.Errorf("create injector DaemonSet: %w", err)
	}
	infof("injector DaemonSet applied: %s pinned to %d connector node(s) via %s", poolInjectorDaemonSet, len(nodes), poolNodeLabel)
	return len(nodes), nil
}

// buildInjectorDaemonSet builds the persistent per-node injector using the
// multi-arch harnessctl image (arch chosen by the node kubelet).
func buildInjectorDaemonSet(cfg Config) *appsv1.DaemonSet {
	labels := map[string]string{"app": poolInjectorDaemonSet, poolInjectorLabel: "true"}
	mounts := []corev1.VolumeMount{{Name: "pool-sockets", MountPath: "/pool-sockets"}}
	vols := []corev1.Volume{{
		Name:         "pool-sockets",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: poolSocketRoot, Type: ptr(corev1.HostPathDirectoryOrCreate)}},
	}}
	// Mount the mongo mTLS client cert (when the chart ships one) so an in-injector
	// reconcile can authenticate with X.509 directly — no manual `kubectl cp` of
	// certs. optional=true keeps the DS schedulable on plain-auth clusters.
	if cfg.MongoTLSSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "mongo-certs", MountPath: "/etc/mongo-certs", ReadOnly: true})
		vols = append(vols, corev1.Volume{
			Name:         "mongo-certs",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: cfg.MongoTLSSecret, Optional: ptr(true)}},
		})
	}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: poolInjectorDaemonSet, Namespace: cfg.NVSNamespace, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{poolNodeLabel: "true"},
					Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					// Cluster already carries ghcr-secret for private nvidia/* packages.
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "ghcr-secret"}},
					Containers: []corev1.Container{{
						Name:            "injector",
						Image:           cfg.injectorPodImage(),
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"sleep", "infinity"},
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{RunAsUser: ptr(int64(0))},
					}},
					Volumes: vols,
				},
			},
		},
	}
}
