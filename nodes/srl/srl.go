// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package srl

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/google/shlex"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/srl-labs/containerlab/cert"
	"github.com/srl-labs/containerlab/nodes"
	"github.com/srl-labs/containerlab/runtime"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
)

const (
	srlDefaultType = "ixrd2"

	readyTimeout = time.Minute * 2 // max wait time for node to boot
	retryTimer   = time.Second
	// additional config that clab adds on top of the factory config
	srlConfigCmdsTpl = `set / system tls server-profile clab-profile
set / system tls server-profile clab-profile key "{{ .TLSKey }}"
set / system tls server-profile clab-profile certificate "{{ .TLSCert }}"
{{- if .TLSAnchor }}
set / system tls server-profile clab-profile authenticate-client true
set / system tls server-profile clab-profile trust-anchor "{{ .TLSAnchor }}"
{{- else }}
set / system tls server-profile clab-profile authenticate-client false
{{- end }}
set / system gnmi-server admin-state enable network-instance mgmt admin-state enable tls-profile clab-profile
set / system json-rpc-server admin-state enable network-instance mgmt http admin-state enable
set / system json-rpc-server admin-state enable network-instance mgmt https admin-state enable tls-profile clab-profile
set / system lldp admin-state enable
set / system aaa authentication idle-timeout 7200
commit save`
)

var (
	srlSysctl = map[string]string{
		"net.ipv4.ip_forward":              "0",
		"net.ipv6.conf.all.disable_ipv6":   "0",
		"net.ipv6.conf.all.accept_dad":     "0",
		"net.ipv6.conf.default.accept_dad": "0",
		"net.ipv6.conf.all.autoconf":       "0",
		"net.ipv6.conf.default.autoconf":   "0",
	}

	srlTypes = map[string]string{
		"ixr6":  "7250IXR6.yml",
		"ixr10": "7250IXR10.yml",
		"ixrd1": "7220IXRD1.yml",
		"ixrd2": "7220IXRD2.yml",
		"ixrd3": "7220IXRD3.yml",
		"ixrh2": "7220IXRH2.yml",
		"ixrh3": "7220IXRH3.yml",
	}

	srlEnv = map[string]string{"SRLINUX": "1"}

	//go:embed topology/*
	topologies embed.FS

	saveCmd              = []string{"sr_cli", "-d", "tools", "system", "configuration", "save"}
	mgmtServerRdyCmd, _  = shlex.Split("sr_cli -d info from state system app-management application mgmt_server state | grep running")
	commitCompleteCmd, _ = shlex.Split("sr_cli -d info from state system configuration commit 1 status | grep complete")

	srlCfgTpl, _ = template.New("srl-tls-profile").Parse(srlConfigCmdsTpl)
)

func init() {
	nodes.Register(nodes.NodeKindSRL, func() nodes.Node {
		return new(srl)
	})
}

type srl struct {
	cfg     *types.NodeConfig
	runtime runtime.ContainerRuntime
}

func (s *srl) Init(cfg *types.NodeConfig, opts ...nodes.NodeOption) error {
	s.cfg = cfg
	for _, o := range opts {
		o(s)
	}

	if s.cfg.NodeType == "" {
		s.cfg.NodeType = srlDefaultType
	}

	if _, found := srlTypes[s.cfg.NodeType]; !found {
		keys := make([]string, 0, len(srlTypes))
		for key := range srlTypes {
			keys = append(keys, key)
		}
		return fmt.Errorf("wrong node type. '%s' doesn't exist. should be any of %s", s.cfg.NodeType, strings.Join(keys, ", "))
	}

	// the addition touch is needed to support non docker runtimes
	s.cfg.Cmd = "sudo bash -c 'touch /.dockerenv && /opt/srlinux/bin/sr_linux'"

	s.cfg.Env = utils.MergeStringMaps(srlEnv, s.cfg.Env)

	// if user was not initialized to a value, use root
	if s.cfg.User == "" {
		s.cfg.User = "0:0"
	}
	for k, v := range srlSysctl {
		s.cfg.Sysctls[k] = v
	}

	if s.cfg.License != "" {
		// we mount a fixed path node.Labdir/license.key as the license referenced in topo file will be copied to that path
		s.cfg.Binds = append(s.cfg.Binds, fmt.Sprint(filepath.Join(s.cfg.LabDir, "license.key"), ":/opt/srlinux/etc/license.key:ro"))
	}

	// mount config directory
	cfgPath := filepath.Join(s.cfg.LabDir, "config")
	s.cfg.Binds = append(s.cfg.Binds, fmt.Sprint(cfgPath, ":/etc/opt/srlinux/:rw"))

	// mount srlinux topology
	topoPath := filepath.Join(s.cfg.LabDir, "topology.yml")
	s.cfg.Binds = append(s.cfg.Binds, fmt.Sprint(topoPath, ":/tmp/topology.yml:ro"))

	return nil
}

func (s *srl) Config() *types.NodeConfig { return s.cfg }

func (s *srl) PreDeploy(configName, labCADir, labCARoot string) error {
	utils.CreateDirectory(s.cfg.LabDir, 0777)
	// retrieve node certificates
	nodeCerts, err := cert.RetrieveNodeCertData(s.cfg, labCADir)
	// if not available on disk, create cert in next step
	if err != nil {
		// create CERT
		certTpl, err := template.New("node-cert").Parse(cert.NodeCSRTempl)
		if err != nil {
			log.Errorf("failed to parse Node CSR Template: %v", err)
		}
		certInput := cert.CertInput{
			Name:     s.cfg.ShortName,
			LongName: s.cfg.LongName,
			Fqdn:     s.cfg.Fqdn,
			Prefix:   configName,
		}
		nodeCerts, err = cert.GenerateCert(
			path.Join(labCARoot, "root-ca.pem"),
			path.Join(labCARoot, "root-ca-key.pem"),
			certTpl,
			certInput,
			path.Join(labCADir, certInput.Name),
		)
		if err != nil {
			log.Errorf("failed to generate certificates for node %s: %v", s.cfg.ShortName, err)
		}
		log.Debugf("%s CSR: %s", s.cfg.ShortName, string(nodeCerts.Csr))
		log.Debugf("%s Cert: %s", s.cfg.ShortName, string(nodeCerts.Cert))
		log.Debugf("%s Key: %s", s.cfg.ShortName, string(nodeCerts.Key))
	}
	s.cfg.TLSCert = string(nodeCerts.Cert)
	s.cfg.TLSKey = string(nodeCerts.Key)

	// Create appmgr subdir for agent specs and copy files, if needed
	if s.cfg.Extras != nil && len(s.cfg.Extras.SRLAgents) != 0 {
		agents := s.cfg.Extras.SRLAgents
		appmgr := filepath.Join(s.cfg.LabDir, "config/appmgr/")
		utils.CreateDirectory(appmgr, 0777)

		for _, fullpath := range agents {
			basename := filepath.Base(fullpath)
			dst := filepath.Join(appmgr, basename)
			if err := utils.CopyFile(fullpath, dst, 0644); err != nil {
				return fmt.Errorf("agent copy src %s -> dst %s failed %v", fullpath, dst, err)
			}
		}
	}

	return createSRLFiles(s.cfg)
}

func (s *srl) Deploy(ctx context.Context) error {
	_, err := s.runtime.CreateContainer(ctx, s.cfg)
	return err
}

func (s *srl) PostDeploy(ctx context.Context, _ map[string]nodes.Node) error {
	// only perform postdeploy additional config provisioning if there is not startup nor existing config
	if s.cfg.StartupConfig != "" || utils.FileExists(filepath.Join(s.cfg.LabDir, "config", "config.json")) {
		return nil
	}

	log.Infof("Running postdeploy actions for Nokia SR Linux '%s' node", s.cfg.ShortName)

	return s.addDefaultConfig(ctx)
}

func (s *srl) GetImages() map[string]string {
	return map[string]string{
		nodes.ImageKey: s.cfg.Image,
	}
}

func (*srl) WithMgmtNet(*types.MgmtNet)               {}
func (s *srl) WithRuntime(r runtime.ContainerRuntime) { s.runtime = r }
func (s *srl) GetRuntime() runtime.ContainerRuntime   { return s.runtime }

func (s *srl) Delete(ctx context.Context) error {
	return s.runtime.DeleteContainer(ctx, s.Config().LongName)
}

func (s *srl) SaveConfig(ctx context.Context) error {
	stdout, stderr, err := s.runtime.Exec(ctx, s.cfg.LongName, saveCmd)
	if err != nil {
		return fmt.Errorf("%s: failed to execute cmd: %v", s.cfg.ShortName, err)
	}

	if len(stderr) > 0 {
		return fmt.Errorf("%s errors: %s", s.cfg.ShortName, string(stderr))
	}

	log.Infof("saved SR Linux configuration from %s node. Output:\n%s", s.cfg.ShortName, string(stdout))

	return nil
}

// Ready returns when the node boot sequence reached the stage when it is ready to accept config commands
// returns an error if not ready by the expiry of the timer readyTimeout.
func (s *srl) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	var stdout, stderr []byte
	var err error

	log.Debugf("Waiting for SR Linux node %q to boot...", s.cfg.ShortName)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for SR Linux node %s to boot: %v", s.cfg.ShortName, err)
		default:
			// two commands are checked, first if the mgmt_server is running
			stdout, stderr, err = s.GetRuntime().Exec(ctx, s.cfg.LongName, mgmtServerRdyCmd)
			if err != nil {
				time.Sleep(retryTimer)
				continue
			}
			if len(stderr) != 0 {
				log.Debugf("error during checking SR Linux boot status: %s", string(stderr))
				time.Sleep(retryTimer)
				continue
			}
			if !bytes.Contains(stdout, []byte("running")) {
				time.Sleep(retryTimer)
				continue
			}

			// and then if the initial commit completes
			stdout, stderr, err = s.GetRuntime().Exec(ctx, s.cfg.LongName, commitCompleteCmd)
			if err != nil {
				time.Sleep(retryTimer)
				continue
			}

			if len(stderr) != 0 {
				log.Debugf("error during checking SR Linux boot status: %s", string(stderr))
				time.Sleep(retryTimer)
				continue
			}

			if !bytes.Contains(stdout, []byte("complete")) {
				log.Debugf("node %s not yet ready", s.cfg.ShortName)
				time.Sleep(retryTimer)
				continue
			}
			log.Debugf("Node %s booted", s.cfg.ShortName)
			return nil
		}
	}
}

//

func createSRLFiles(nodeCfg *types.NodeConfig) error {
	log.Debugf("Creating directory structure for SRL container: %s", nodeCfg.ShortName)
	var src string
	var dst string

	if nodeCfg.License != "" {
		// copy license file to node specific directory in lab
		src = nodeCfg.License
		dst = filepath.Join(nodeCfg.LabDir, "license.key")
		if err := utils.CopyFile(src, dst, 0644); err != nil {
			return fmt.Errorf("CopyFile src %s -> dst %s failed %v", src, dst, err)
		}
		log.Debugf("CopyFile src %s -> dst %s succeeded", src, dst)
	}

	// generate SRL topology file
	err := generateSRLTopologyFile(nodeCfg.NodeType, nodeCfg.LabDir, nodeCfg.Index)
	if err != nil {
		return err
	}

	utils.CreateDirectory(path.Join(nodeCfg.LabDir, "config"), 0777)

	// generate a startup config file
	// if the node has a `startup-config:` statement, the file specified in that section
	// will be used as a template in GenerateConfig()
	if nodeCfg.StartupConfig != "" {
		dst = filepath.Join(nodeCfg.LabDir, "config", "config.json")

		log.Debugf("Reading startup-config %s", nodeCfg.StartupConfig)

		c, err := os.ReadFile(nodeCfg.StartupConfig)
		if err != nil {
			return err
		}

		cfgTemplate := string(c)

		err = nodeCfg.GenerateConfig(dst, cfgTemplate)
		if err != nil {
			log.Errorf("node=%s, failed to generate config: %v", nodeCfg.ShortName, err)
		}
	}

	return err
}

type mac struct {
	MAC string
}

func generateSRLTopologyFile(nodeType, labDir string, _ int) error {
	dst := filepath.Join(labDir, "topology.yml")

	tpl, err := template.ParseFS(topologies, "topology/"+srlTypes[nodeType])
	if err != nil {
		return errors.Wrap(err, "failed to get srl topology file")
	}

	// generate random bytes to use in the 2-3rd bytes of a base mac
	// this ensures that different srl nodes will have different macs for their ports
	buf := make([]byte, 2)
	_, err = rand.Read(buf)
	if err != nil {
		return err
	}
	m := fmt.Sprintf("02:%02x:%02x:00:00:00", buf[0], buf[1])

	mac := mac{
		MAC: m,
	}
	log.Debug(mac, dst)
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	return tpl.Execute(f, mac)
}

// addDefaultConfig adds srl default configuration such as tls certs and gnmi/json-rpc
func (s *srl) addDefaultConfig(ctx context.Context) error {
	// start waiting for initial commit and mgmt server ready
	if err := s.Ready(ctx); err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	err := srlCfgTpl.Execute(buf, s.cfg)
	if err != nil {
		return err
	}

	log.Debugf("Node %q additional config:\n%s", s.cfg.ShortName, buf.String())
	_, _, err = s.runtime.Exec(ctx, s.cfg.LongName, []string{
		"bash",
		"-c",
		fmt.Sprintf("echo '%s' > /tmp/clab-config", buf.String()),
	})

	if err != nil {
		return err
	}

	stdout, stderr, err := s.runtime.Exec(ctx, s.cfg.LongName, []string{
		"bash",
		"-c",
		"sr_cli -ed < tmp/clab-config",
	})

	if err != nil {
		return err
	}

	log.Debugf("node %s. stdout: %s, stderr: %s", s.cfg.ShortName, stdout, stderr)

	return nil
}
