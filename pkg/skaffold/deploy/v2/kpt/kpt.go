/*
Copyright 2021 The Skaffold Authors

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

package kpt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/kustomize/kyaml/fn/framework"
	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/access"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/debug"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/kubectl"
	deployutil "github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/util"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/event"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/graph"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/instrumentation"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/manifest"
	kstatus "github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/status"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/log"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/render/kptfile"
	latestV2 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest/v2"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/status"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/sync"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
)

const (
	deployerName = "kptV2"
	defaultNs    = "default"
)

var (
	openFile    = os.Open
	kptInitFunc = kptfileInitIfNot
)

// Deployer deploys workflows with kpt CLI
type Deployer struct {
	*latestV2.KptV2Deploy
	applyDir string

	accessor      access.Accessor
	logger        log.Logger
	debugger      debug.Debugger
	statusMonitor status.Monitor
	syncer        sync.Syncer

	podSelector    *kubernetes.ImageList
	originalImages []graph.Artifact

	insecureRegistries map[string]bool
	labels             map[string]string
	globalConfig       string
	kubeContext        string
	kubeConfig         string
	namespace          string
}

type Config interface {
	kubectl.Config
	kstatus.Config
}

// NewDeployer generates a new Deployer object contains the kptDeploy schema.
func NewDeployer(cfg Config, labels map[string]string, provider deploy.ComponentProvider, d *latestV2.KptV2Deploy,
	opts config.SkaffoldOptions) *Deployer {
	podSelector := kubernetes.NewImageList()
	if opts.InventoryNamespace != "" {
		d.InventoryNamespace = opts.InventoryNamespace
	}
	if opts.InventoryID != "" {
		d.InventoryID = opts.InventoryID
	}
	if opts.InventoryName != "" {
		d.Name = opts.InventoryName
	}
	return &Deployer{
		KptV2Deploy: d,
		applyDir:    d.Dir,
		podSelector: podSelector,
		// TODO: use pkg/skaffold/deploy/component/kubernetes. need cherry-picking from master.
		accessor:           provider.Accessor.GetKubernetesAccessor(cfg, podSelector),
		debugger:           provider.Debugger.GetKubernetesDebugger(podSelector),
		logger:             provider.Logger.GetKubernetesLogger(podSelector),
		statusMonitor:      provider.Monitor.GetKubernetesMonitor(cfg),
		syncer:             provider.Syncer.GetKubernetesSyncer(podSelector),
		insecureRegistries: cfg.GetInsecureRegistries(),
		labels:             labels,
		globalConfig:       cfg.GlobalConfig(),
		kubeContext:        cfg.GetKubeContext(),
		kubeConfig:         cfg.GetKubeConfig(),
		namespace:          cfg.GetKubeNamespace(),
	}
}

func (k *Deployer) GetAccessor() access.Accessor {
	return k.accessor
}

func (k *Deployer) GetDebugger() debug.Debugger {
	return k.debugger
}

func (k *Deployer) GetLogger() log.Logger {
	return k.logger
}

func (k *Deployer) GetStatusMonitor() status.Monitor {
	return k.statusMonitor
}

func (k *Deployer) GetSyncer() sync.Syncer {
	return k.syncer
}

// TrackBuildArtifacts registers build artifacts to be tracked by a Deployer
func (k *Deployer) TrackBuildArtifacts(artifacts []graph.Artifact) {
	deployutil.AddTagsToPodSelector(artifacts, k.originalImages, k.podSelector)
	k.logger.RegisterArtifacts(artifacts)
}

func (k *Deployer) getManifests(ctx context.Context) (manifest.ManifestList, error) {
	cmd := exec.CommandContext(
		ctx, "kpt", "fn", "source", k.applyDir)
	buf, err := util.RunCmdOut(cmd)
	if err != nil {
		return nil, sourceErr(err, k.applyDir)
	}
	input := bytes.NewBufferString(string(buf))
	rl := framework.ResourceList{
		Reader: input,
	}
	// Manipulate the kustomize "Rnode"(Kustomize term) and pulls out the "Items"
	// from ResourceLists.
	if err := rl.Read(); err != nil {
		return nil, sourceErr(err, k.applyDir)
	}
	var newBuf []byte
	for i := range rl.Items {
		item, err := rl.Items[i].String()
		if err != nil {
			return nil, sourceErr(err, k.applyDir)
		}
		newBuf = append(newBuf, []byte(item)...)
	}
	manifests := manifest.ManifestList{}
	if len(buf) > 0 {
		manifests.Append(newBuf)
	}
	return manifests, nil
}

// kptfileInitIfNot guarantees the Kptfile is valid.
func kptfileInitIfNot(ctx context.Context, out io.Writer, k *Deployer) error {
	kptFilePath := filepath.Join(k.applyDir, kptfile.KptFileName)
	if _, err := os.Stat(kptFilePath); os.IsNotExist(err) {
		_, endTrace := instrumentation.StartTrace(ctx, "Deploy_InitKptfile")
		cmd := exec.CommandContext(ctx, "kpt", "pkg", "init", k.applyDir)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := util.RunCmd(cmd); err != nil {
			endTrace(instrumentation.TraceEndError(err))
			return pkgInitErr(err, k.applyDir)
		}
	}
	file, err := openFile(kptFilePath)
	if err != nil {
		return openFileErr(err, kptFilePath)
	}
	defer file.Close()
	kfConfig := &kptfile.KptFile{}
	if err := yaml.NewDecoder(file).Decode(&kfConfig); err != nil {
		return parseFileErr(err, kptFilePath)
	}
	// Kptfile may already exist but do not contain the "Inventory" field, which is mandatory for `kpt live apply`.
	// This case happens when Kptfile is created by `kpt pkg init` and can be resolved by running `kpt live init`.
	// If "Inventory" already exist, running `kpt live init` raises error.
	if kfConfig.Inventory == nil {
		_, endTrace := instrumentation.StartTrace(ctx, "Deploy_InitKptfileInventory")
		args := []string{"live", "init", k.applyDir}
		args = append(args, k.KptV2Deploy.Flags...)
		if k.Name != "" {
			args = append(args, "--name", k.Name)
		}
		if k.InventoryID != "" {
			args = append(args, "--inventory-id", k.InventoryID)
		}
		if k.namespace != "" {
			args = append(args, "--namespace", k.namespace)
		} else if k.InventoryNamespace != "" {
			args = append(args, "--namespace", k.InventoryNamespace)
		}
		if k.Force {
			args = append(args, "--force", "true")
		}
		cmd := exec.CommandContext(ctx, "kpt", args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := util.RunCmd(cmd); err != nil {
			endTrace(instrumentation.TraceEndError(err))
			return liveInitErr(err, k.applyDir)
		}
	} else {
		if k.InventoryID != "" && k.InventoryID != kfConfig.Inventory.InventoryID {
			logrus.Warnf("Updating Kptfile inventory from %v to %v", kfConfig.Inventory.InventoryID, k.InventoryID)
			kfConfig.Inventory.InventoryID = k.InventoryID
		}
		if k.Name != "" && k.Name != kfConfig.Inventory.Name {
			logrus.Warnf("Updating Kptfile name from %v to %v", kfConfig.Inventory.Name, k.Name)
			kfConfig.Inventory.Name = k.Name
		}
		// Set the namespace to be a valid kubernetes namespace value. If not specified, the value shall be "default".
		if k.namespace == "" {
			k.namespace = defaultNs
		}
		if k.InventoryNamespace == "" {
			k.InventoryNamespace = defaultNs
		}
		if k.namespace != kfConfig.Inventory.Namespace {
			logrus.Warnf("Updating Kptfile namespace from %v to %v", kfConfig.Inventory.Namespace, k.namespace)
			kfConfig.Inventory.Namespace = k.namespace
		} else if k.InventoryNamespace != kfConfig.Inventory.Namespace {
			logrus.Warnf("Updating Kptfile namespace from %v to %v", kfConfig.Inventory.Namespace, k.InventoryNamespace)
			kfConfig.Inventory.Namespace = k.InventoryNamespace
		}
		configByte, err := yaml.Marshal(kfConfig)
		if err != nil {
			return err
		}
		if err = ioutil.WriteFile(kptFilePath, configByte, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (k *Deployer) Deploy(ctx context.Context, out io.Writer, builds []graph.Artifact) ([]string, error) {
	if err := kptInitFunc(ctx, out, k); err != nil {
		return nil, err
	}

	instrumentation.AddAttributesToCurrentSpanFromContext(ctx, map[string]string{
		"DeployerType": deployerName,
	})
	_, endTrace := instrumentation.StartTrace(ctx, "Deploy_ReadHydratedManifests")
	manifests, err := k.getManifests(ctx)
	if err != nil {
		event.DeployInfoEvent(fmt.Errorf("could not read the hydrated manifest from %v: %w", k.applyDir, err))
	}
	endTrace()

	_, endTrace = instrumentation.StartTrace(ctx, "Deploy_CollectNamespaces")
	namespaces, err := manifests.CollectNamespaces()
	if err != nil {
		event.DeployInfoEvent(fmt.Errorf("could not fetch deployed resource namespace. "+
			"This might cause port-forward and deploy health-check to fail: %w", err))
	}
	endTrace()

	childCtx, endTrace := instrumentation.StartTrace(ctx, "Deploy_execKptCommand")
	args := []string{"live", "apply", k.applyDir}

	args = append(args, k.Flags...)
	args = append(args, k.ApplyFlags...)
	cmd := exec.CommandContext(childCtx, "kpt", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := util.RunCmd(cmd); err != nil {
		endTrace(instrumentation.TraceEndError(err))
		return nil, liveApplyErr(err, k.applyDir)
	}
	k.TrackBuildArtifacts(builds)
	endTrace()
	return namespaces, nil
}

// TODO(yuwenma)[07/23/22]: remove Render func from all deployers and deployerMux.
func (k *Deployer) Render(context.Context, io.Writer, []graph.Artifact, bool, string) error {
	return fmt.Errorf("shall not be called")
}

// Dependencies is the v1 function required by "deployer" interface. It shall be no-op for v2 deployers.
func (k *Deployer) Dependencies() ([]string, error) {
	return []string{}, nil
}

// Cleanup deletes what was deployed by calling `kpt live destroy`.
func (k *Deployer) Cleanup(ctx context.Context, out io.Writer) error {
	instrumentation.AddAttributesToCurrentSpanFromContext(ctx, map[string]string{
		"DeployerType": deployerName,
	})
	if err := kptInitFunc(ctx, out, k); err != nil {
		return err
	}

	args := []string{"live", "destroy", k.applyDir}
	args = append(args, k.Flags...)
	cmd := exec.CommandContext(ctx, "kpt", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := util.RunCmd(cmd); err != nil {
		return liveDestroyErr(err, k.applyDir)
	}

	return nil
}
