// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package router

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"go.ate.dev/substrate/cmd/atenet/internal/router/extproc"
	"go.ate.dev/substrate/cmd/atenet/internal/router/ingress"
)

var BuildTag = "dev"

type DashboardContext struct {
	BuildTag        string                `json:"build_tag"`
	RouterClusterIP string                `json:"router_cluster_ip"`
	Namespace       string                `json:"namespace"`
	Mode            Mode                  `json:"mode"`
	HttpsPort       int                   `json:"port_https"`
	XdsPort         int                   `json:"port_xds"`
	ExtprocPort     int                   `json:"port_extproc"`
	StatusPort      int                   `json:"status_port"`
	Args            string                `json:"args"`
	Flags           map[string]string     `json:"flags"`
	Queries         []FormattedQuery      `json:"queries"`
	Health          RouterHealthReport    `json:"health"`
	Parking         ingress.ParkingStatus `json:"parking"`
}

type FormattedQuery struct {
	Timestamp string `json:"timestamp"`
	Client    string `json:"client"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	Method    string `json:"method"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Duration  string `json:"duration"`
}

// listenStatus binds the /statusz listener on --status-address and
// --status-port.
func (c routerConfig) listenStatus() (net.Listener, error) {
	addr := net.JoinHostPort(c.StatusAddr, strconv.Itoa(c.StatusPort))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed binding Router HTTP status server address %q: %w", addr, err)
	}
	return lis, nil
}

// getRouterIP reads the Service ClusterIP from the env var the kubelet injects
// for Services in the pod's own namespace (not cfg.Namespace). The router's
// RBAC does not grant Services get, so an API lookup would always fail.
// The var is set once at pod start, only if the Service already existed and
// enableServiceLinks is not false, so "Unavailable" (or a stale IP after a
// Service recreate) is possible.
func (s *RouterServer) getRouterIP(_ context.Context) string {
	if s.clientset == nil {
		return "Offline Mode (No Cluster IP)"
	}

	// The kubelet names the var after the Service: upper case, dashes to underscores.
	env := strings.ToUpper(strings.ReplaceAll(s.cfg.RouterServiceName, "-", "_")) + "_SERVICE_HOST"
	if ip := os.Getenv(env); ip != "" {
		return ip
	}
	return "Unavailable"
}

func (s *RouterServer) handleStatusz(w http.ResponseWriter, req *http.Request) {
	routerIP := s.getRouterIP(req.Context())

	buildInfo := BuildTag
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				buildInfo += fmt.Sprintf(" (rev: %s)", setting.Value)
			}
		}
	}

	argsStr := strings.Join(os.Args, " ")

	flagsMap := make(map[string]string)
	if s.Cmd != nil {
		s.Cmd.Flags().VisitAll(func(f *pflag.Flag) {
			flagsMap[f.Name] = f.Value.String()
		})
	}

	var rawQueries []extproc.RecordedQuery
	if s.extprocSrv != nil {
		rawQueries = s.extprocSrv.Queries()
	}

	formattedQueries := make([]FormattedQuery, len(rawQueries))
	for i, q := range rawQueries {
		formattedQueries[i] = FormattedQuery{
			Timestamp: q.Timestamp.Format(time.RFC3339),
			Client:    q.Client,
			Host:      q.Host,
			Path:      q.Path,
			Method:    q.Method,
			Action:    q.Action,
			Target:    q.Target,
			Duration:  fmt.Sprintf("%.2f ms", float64(q.Duration.Microseconds())/1000),
		}
	}

	var hr RouterHealthReport
	if s.health != nil {
		hr = s.health.Report()
	}

	// Parking belongs to the ingress handler; an egress-only instance has none.
	var parking ingress.ParkingStatus
	if s.ingressHandler != nil {
		parking = s.ingressHandler.ParkingStatus()
	}

	data := DashboardContext{
		BuildTag:        buildInfo,
		RouterClusterIP: routerIP,
		Namespace:       s.cfg.Namespace,
		Mode:            s.cfg.Mode,
		HttpsPort:       s.cfg.HttpsPort,
		XdsPort:         s.cfg.XdsPort,
		ExtprocPort:     s.cfg.ExtprocPort,
		StatusPort:      s.cfg.StatusPort,
		Args:            argsStr,
		Flags:           flagsMap,
		Queries:         formattedQueries,
		Health:          hr,
		Parking:         parking,
	}

	accept := req.Header.Get("Accept")
	formatParam := req.URL.Query().Get("format")

	if strings.Contains(accept, "application/json") || formatParam == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(data)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, err := template.New("dashboard").Parse(dashboardHTML)
	if err != nil {
		http.Error(w, fmt.Sprintf("Template parsing failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = tmpl.Execute(w, data)
}

//go:embed dashboard.html
var dashboardHTML string
