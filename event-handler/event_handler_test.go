/*
Copyright 2015-2021 Gravitational, Inc.

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

package main

import (
	"encoding/json"
	"os"
	"os/user"
	"testing"
	"time"

	"github.com/gravitational/teleport-plugins/lib"
	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/teleport-plugins/lib/testing/integration"
	"github.com/gravitational/teleport/api/types"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type EventHandlerSuite struct {
	integration.SSHSetup
	appConfig   StartCmdConfig
	fakeFluentd *FakeFluentd

	userNames struct {
		ruler  string
		plugin string
	}

	me             *user.User
	clients        map[string]*integration.Client
	teleportConfig lib.TeleportConfig
}

type event struct {
	Type string `json:"event,omitempty"`
}

func TestEventHandler(t *testing.T) { suite.Run(t, &EventHandlerSuite{}) }

func (s *EventHandlerSuite) SetupSuite() {
	var err error
	t := s.T()

	s.SSHSetup.SetupSuite()
	s.SSHSetup.SetupService()

	me, err := user.Current()
	require.NoError(t, err)

	s.clients = make(map[string]*integration.Client)

	// Set up the user who has an access to all kinds of resources.

	s.userNames.ruler = me.Username + "-ruler@example.com"
	client, err := s.Integration.MakeAdmin(s.Context(), s.Auth, s.userNames.ruler)
	require.NoError(t, err)
	s.clients[s.userNames.ruler] = client

	var bootstrap integration.Bootstrap

	// Set up plugin user.

	role, err := bootstrap.AddRole("access-event-handler", types.RoleSpecV4{
		Allow: types.RoleConditions{
			Rules: []types.Rule{
				types.NewRule("event", []string{"list", "read"}),
				types.NewRule("session", []string{"list", "read"}),
			},
			Logins: []string{me.Username},
		},
	})
	require.NoError(t, err)

	user, err := bootstrap.AddUserWithRoles("access-event-handler", role.GetName())
	require.NoError(t, err)
	s.userNames.plugin = user.GetName()

	// Bake all the resources.

	err = s.Integration.Bootstrap(s.Context(), s.Auth, bootstrap.Resources())
	require.NoError(t, err)

	// Initialize the clients.

	identityPath, err := s.Integration.Sign(s.Context(), s.Auth, s.userNames.plugin)
	require.NoError(t, err)

	s.me = me
	s.teleportConfig.Addr = s.Auth.AuthAddr().String()
	s.teleportConfig.Identity = identityPath

	s.SetContextTimeout(5 * time.Minute)
}

func (s *EventHandlerSuite) SetupTest() {
	t := s.T()

	logger.Setup(logger.Config{Severity: "debug"})

	fd, err := NewFakeFluentd()
	require.NoError(t, err)
	s.fakeFluentd = fd
	s.fakeFluentd.Start()
	t.Cleanup(s.fakeFluentd.Close)

	startTime := time.Now().Add(-time.Minute)

	conf := StartCmdConfig{
		TeleportConfig: TeleportConfig{
			TeleportAddr:         s.teleportConfig.Addr,
			TeleportIdentityFile: s.teleportConfig.Identity,
		},
		FluentdConfig: s.fakeFluentd.GetClientConfig(),
		IngestConfig: IngestConfig{
			StorageDir:       os.TempDir(),
			Timeout:          time.Second,
			BatchSize:        100,
			Concurrency:      5,
			StartTime:        &startTime,
			SkipSessionTypes: map[string]struct{}{"print": {}},
		},
	}

	conf.FluentdURL = s.fakeFluentd.GetURL()
	conf.FluentdSessionURL = conf.FluentdURL + "/session"

	s.appConfig = conf
}

func (s *EventHandlerSuite) startApp() {
	t := s.T()
	t.Helper()

	app, err := NewApp(&s.appConfig)
	require.NoError(t, err)

	s.StartApp(app)
}

func (s *EventHandlerSuite) ruler() *integration.Client {
	return s.clients[s.userNames.ruler]
}

func (s *EventHandlerSuite) TestEvents() {
	t := s.T()

	s.startApp()

	err := s.ruler().CreateUser(s.Context(), &types.UserV2{
		Metadata: types.Metadata{
			Name: "fake-ruler",
		},
		Spec: types.UserSpecV2{
			Roles: []string{"access-event-handler"},
		},
	})
	require.NoError(t, err)

	// Test bootstrap events

	evt, err := s.fakeFluentd.GetMessage(s.Context())
	require.NoError(t, err)
	require.Contains(t, evt, `"event":"role.created"`)
	require.Contains(t, evt, `"name":"integration-admin"`)

	evt, err = s.fakeFluentd.GetMessage(s.Context())
	require.NoError(t, err)
	require.Contains(t, evt, `"event":"user.create"`)
	require.Contains(t, evt, `"name":"`+s.userNames.ruler+`"`)
	require.Contains(t, evt, `"roles":["integration-admin"]`)

	evt, err = s.fakeFluentd.GetMessage(s.Context())
	require.NoError(t, err)
	require.Contains(t, evt, `"event":"role.created"`)
	require.Contains(t, evt, `"name":"access-event-handler"`)

	evt, err = s.fakeFluentd.GetMessage(s.Context())
	require.NoError(t, err)
	require.Contains(t, evt, `"event":"user.create"`)
	require.Contains(t, evt, `"name":"`+s.userNames.plugin+`"`)
	require.Contains(t, evt, `"roles":["access-event-handler"]`)

	evt, err = s.fakeFluentd.GetMessage(s.Context())
	require.NoError(t, err)
	require.Contains(t, evt, `"event":"user.create"`)
	require.Contains(t, evt, `"name":"fake-ruler"`)
	require.Contains(t, evt, `"roles":["access-event-handler"]`)

	// Test session ingestion
	tshCmd := s.Integration.NewTsh(s.Proxy.WebAndSSHProxyAddr(), s.teleportConfig.Identity)
	cmd := tshCmd.SSHCommand(s.Context(), s.me.Username+"@localhost")

	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err)

	err = cmd.Start()
	require.NoError(t, err)

	_, err = stdinPipe.Write([]byte("exit\n"))
	require.NoError(t, err)

	err = cmd.Wait()
	require.NoError(t, err)

	err = stdinPipe.Close()
	require.NoError(t, err)

	// Our test session is very simple. There would be to copies of the same messages: one copy is supposed to be received
	// via audit log, other one - via session log.
	counters := make(map[string]int)
	for i := 0; i < 8; i++ {
		msg, err := s.fakeFluentd.GetMessage(s.Context())
		require.NoError(t, err)

		var e event
		err = json.Unmarshal([]byte(msg), &e)
		require.NoError(t, err)
		counters[e.Type]++
	}

	require.Equal(t, counters["session.start"], 2)
	require.Equal(t, counters["session.leave"], 2)
	require.Equal(t, counters["session.end"], 2)

	// That's the difference in channels
	require.Equal(t, counters["session.data"], 1)
	require.Equal(t, counters["session.upload"], 1)
}
