/*
Copyright 2021 Gravitational, Inc.

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

package tctl

import (
	"context"
	"os/exec"
	"regexp"

	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"
)

var regexpStatusCAPin = regexp.MustCompile(`CA pin +(sha256:[a-zA-Z0-9]+)`)

// Tctl is a runner of tctl command.
type Tctl struct {
	Path       string
	ConfigPath string
	AuthServer string
}

// CheckExecutable checks if `tctl` executable exists in the system.
func (tctl Tctl) CheckExecutable() error {
	_, err := exec.LookPath(tctl.cmd())
	return trace.Wrap(err, "tctl executable is not found")
}

// Sign generates Teleport client credentials at a given path.
func (tctl Tctl) Sign(ctx context.Context, username, outPath string) error {
	log := logger.Get(ctx)
	args := append(tctl.baseArgs(),
		"auth",
		"sign",
		"--user",
		username,
		"--format",
		"file",
		"--overwrite",
		"--out",
		outPath,
	)
	cmd := exec.CommandContext(ctx, tctl.cmd(), args...)
	log.Debugf("Running %s", cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.WithError(err).Debug("tctl auth sign failed:", string(output))
		return trace.Wrap(err, "tctl auth sign failed")
	}
	return nil
}

// Create creates or updates a set of Teleport resources.
func (tctl Tctl) Create(ctx context.Context, resources []types.Resource) error {
	log := logger.Get(ctx)
	args := append(tctl.baseArgs(), "create")
	cmd := exec.CommandContext(ctx, tctl.cmd(), args...)
	log.Debugf("Running %s", cmd)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return trace.Wrap(err, "failed to get stdin pipe")
	}
	go func() {
		defer func() {
			if err := stdinPipe.Close(); err != nil {
				log.WithError(trace.Wrap(err)).Error("Failed to close stdin pipe")
			}
		}()
		if err := writeResourcesYAML(stdinPipe, resources); err != nil {
			log.WithError(trace.Wrap(err)).Error("Failed to serialize resources stdin")
		}
	}()
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.WithError(err).Debug("tctl create failed:", string(output))
		return trace.Wrap(err, "tctl create failed")
	}
	return nil
}

// GetAll loads a bunch of Teleport resources by a given query.
func (tctl Tctl) GetAll(ctx context.Context, query string) ([]types.Resource, error) {
	log := logger.Get(ctx)
	args := append(tctl.baseArgs(), "get", query)
	cmd := exec.CommandContext(ctx, tctl.cmd(), args...)

	log.Debugf("Running %s", cmd)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, trace.Wrap(err, "failed to get stdout")
	}
	if err := cmd.Start(); err != nil {
		return nil, trace.Wrap(err, "failed to start tctl")
	}
	resources, err := readResourcesYAMLOrJSON(stdoutPipe)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, trace.Wrap(err)
	}
	return resources, nil
}

// Get loads a singular resource by its kind and name identifiers.
func (tctl Tctl) Get(ctx context.Context, kind, name string) (types.Resource, error) {
	query := kind + "/" + name
	resources, err := tctl.GetAll(ctx, query)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if len(resources) == 0 {
		return nil, trace.NotFound("resource %q is not found", query)
	}
	return resources[0], nil
}

// GetCAPin sets the auth service CA Pin using output from tctl.
func (tctl Tctl) GetCAPin(ctx context.Context) (string, error) {
	log := logger.Get(ctx)

	args := append(tctl.baseArgs(), "status")
	cmd := exec.CommandContext(ctx, tctl.cmd(), args...)

	log.Debugf("Running %s", cmd)
	output, err := cmd.Output()
	if err != nil {
		return "", trace.Wrap(err, "failed to get auth status")
	}

	submatch := regexpStatusCAPin.FindStringSubmatch(string(output))
	if len(submatch) < 2 || submatch[1] == "" {
		return "", trace.Errorf("failed to find CA Pin in auth status")
	}
	return submatch[1], nil
}

func (tctl Tctl) cmd() string {
	if tctl.Path != "" {
		return tctl.Path
	}
	return "tctl"
}

func (tctl Tctl) baseArgs() (args []string) {
	if tctl.ConfigPath != "" {
		args = append(args, "--config", tctl.ConfigPath)
	}
	if tctl.AuthServer != "" {
		args = append(args, "--auth-server", tctl.AuthServer)
	}
	return
}
