/*
Copyright 2014 The Kubernetes Authors.

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

package componentstatus

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"go.etcd.io/etcd/clientv3"
	"google.golang.org/grpc"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/kubernetes/pkg/probe"
	httpprober "k8s.io/kubernetes/pkg/probe/http"
)

const (
	probeTimeOut = 20 * time.Second
)

type ValidatorFn func([]byte) error

type Server struct {
	Addr         string
	Port         int
	Path         string
	EnableHTTPS  bool
	TLSConfig    *tls.Config
	Validate     ValidatorFn
	Prober       httpprober.Prober
	Once         sync.Once
	client       *clientv3.Client
	KineEndpoint string
}

type ServerStatus struct {
	// +optional
	Component string `json:"component,omitempty"`
	// +optional
	Health string `json:"health,omitempty"`
	// +optional
	HealthCode probe.Result `json:"healthCode,omitempty"`
	// +optional
	Msg string `json:"msg,omitempty"`
	// +optional
	Err string `json:"err,omitempty"`
}

func (server *Server) DoServerCheck() (probe.Result, string, error) {

	if server.KineEndpoint != "" {
		return server.DoServerGrpcCheck()
	}

	// setup the prober
	server.Once.Do(func() {
		if server.Prober != nil {
			return
		}
		const followNonLocalRedirects = true
		server.Prober = httpprober.NewWithTLSConfig(server.TLSConfig, followNonLocalRedirects)
	})

	scheme := "http"
	if server.EnableHTTPS {
		scheme = "https"
	}
	url := utilnet.FormatURL(scheme, server.Addr, server.Port, server.Path)

	result, data, err := server.Prober.Probe(url, nil, probeTimeOut)

	if err != nil {
		return probe.Unknown, "", err
	}
	if result == probe.Failure {
		return probe.Failure, string(data), err
	}
	if server.Validate != nil {
		if err := server.Validate([]byte(data)); err != nil {
			return probe.Failure, string(data), err
		}
	}
	return result, string(data), nil
}

const keepaliveTime = 30 * time.Second
const keepaliveTimeout = 10 * time.Second

// dialTimeout is the timeout for failing to establish a connection.
// It is set to 20 seconds as times shorter than that will cause TLS connections to fail
// on heavily loaded arm64 CPUs (issue #64649)
const dialTimeout = 20 * time.Second

func (server *Server) DoServerGrpcCheck() (probe.Result, string, error) {

	var err error
	server.Once.Do(func() {
		if server.client != nil {
			return
		}

		dialOptions := []grpc.DialOption{
			grpc.WithBlock(), // block until the underlying connection is up
			grpc.WithUnaryInterceptor(grpcprom.UnaryClientInterceptor),
			grpc.WithStreamInterceptor(grpcprom.StreamClientInterceptor),
		}

		cfg := clientv3.Config{
			DialTimeout:          dialTimeout,
			DialKeepAliveTime:    keepaliveTime,
			DialKeepAliveTimeout: keepaliveTimeout,
			DialOptions:          dialOptions,
			Endpoints:            []string{server.KineEndpoint},
			//TLS:                  server.TLSConfig,
		}

		server.client, err = clientv3.New(cfg)
	})

	if err != nil {
		return probe.Unknown, "", err
	}

	var result *clientv3.GetResponse
	result, err = server.client.Get(context.TODO(), "/registry"+server.Path)
	if err != nil {
		return probe.Unknown, "", err
	}

	if len(result.Kvs) < 1 {
		return probe.Failure, string(fmt.Sprintf(`/registry/%s key not found`, server.Path)), err
	}

	if server.Validate != nil {
		if err := server.Validate([]byte(result.Kvs[0].Value)); err != nil {
			return probe.Failure, string(result.Kvs[0].Value), err
		}
	}

	return probe.Success, string(result.Kvs[0].Value), nil
}
