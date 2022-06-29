// Licensed to Alexandre VILAIN under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Alexandre VILAIN licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package resource

import (
	"fmt"
	"path"
	"time"

	"github.com/alexandrevilain/temporal-operator/api/v1alpha1"
	"github.com/alexandrevilain/temporal-operator/internal/metadata"
	"github.com/alexandrevilain/temporal-operator/pkg/persistence"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type ConfigmapBuilder struct {
	instance *v1alpha1.TemporalCluster
	scheme   *runtime.Scheme
}

func NewConfigmapBuilder(instance *v1alpha1.TemporalCluster, scheme *runtime.Scheme) *ConfigmapBuilder {
	return &ConfigmapBuilder{
		instance: instance,
		scheme:   scheme,
	}
}

func (b *ConfigmapBuilder) Build() (client.Object, error) {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        b.instance.ChildResourceName(ServiceConfig),
			Namespace:   b.instance.Namespace,
			Labels:      metadata.GetLabels(b.instance.Name, ServiceConfig, b.instance.Spec.Version, b.instance.Labels),
			Annotations: metadata.GetAnnotations(b.instance.Name, b.instance.Annotations),
		},
	}, nil
}

func (b *ConfigmapBuilder) Update(object client.Object) error {
	configMap := object.(*corev1.ConfigMap)

	datastores := map[string]config.DataStore{}
	for _, store := range b.instance.Spec.Datastores {
		datastoreType, err := store.GetDatastoreType()
		if err != nil {
			return err
		}

		cfg := config.DataStore{}
		switch datastoreType {
		case v1alpha1.MySQLDatastore, v1alpha1.PostgresSQLDatastore:
			cfg.SQL = persistence.NewSQLConfigFromDatastoreSpec(&store)
			cfg.SQL.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
		case v1alpha1.CassandraDatastore:
			cfg.Cassandra = persistence.NewCassandraConfigFromDatastoreSpec(&store)
			cfg.Cassandra.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
		case v1alpha1.ElasticsearchDatastore:
			esCfg, err := persistence.NewElasticsearchConfigFromDatastoreSpec(&store)
			if err != nil {
				return fmt.Errorf("can't get elasticsearch config: %w", err)
			}
			cfg.Elasticsearch = esCfg
			cfg.Elasticsearch.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
		}
		datastores[store.Name] = cfg
	}

	temporalCfg := config.Config{
		Global: config.Global{
			Membership: config.Membership{
				MaxJoinDuration:  30 * time.Second,
				BroadcastAddress: "{{ default .Env.POD_IP \"0.0.0.0\" }}",
			},
		},
		Persistence: config.Persistence{
			DefaultStore:            b.instance.Spec.Persistence.DefaultStore,
			VisibilityStore:         b.instance.Spec.Persistence.VisibilityStore,
			AdvancedVisibilityStore: b.instance.Spec.Persistence.AdvancedVisibilityStore,
			NumHistoryShards:        b.instance.Spec.NumHistoryShards,
			DataStores:              datastores,
		},
		Log: log.Config{
			Stdout: true,
			Level:  "info",
		},
		ClusterMetadata: &cluster.Config{
			EnableGlobalNamespace:    false,
			FailoverVersionIncrement: 10,
			MasterClusterName:        b.instance.Name,
			CurrentClusterName:       b.instance.Name,
			ClusterInformation: map[string]cluster.ClusterInformation{
				b.instance.Name: {
					Enabled:                true,
					InitialFailoverVersion: 1,
					RPCAddress:             "127.0.0.1:7233",
				},
			},
		},
		Services: map[string]config.Service{
			common.FrontendServiceName: {
				RPC: config.RPC{
					GRPCPort:        *b.instance.Spec.Services.Frontend.Port,
					MembershipPort:  *b.instance.Spec.Services.Frontend.MembershipPort,
					BindOnLocalHost: false,
					BindOnIP:        "0.0.0.0",
				},
			},
			common.HistoryServiceName: {
				RPC: config.RPC{
					GRPCPort:        *b.instance.Spec.Services.History.Port,
					MembershipPort:  *b.instance.Spec.Services.History.MembershipPort,
					BindOnLocalHost: false,
					BindOnIP:        "0.0.0.0",
				},
			},
			common.MatchingServiceName: {
				RPC: config.RPC{
					GRPCPort:        *b.instance.Spec.Services.Matching.Port,
					MembershipPort:  *b.instance.Spec.Services.Matching.MembershipPort,
					BindOnLocalHost: false,
					BindOnIP:        "0.0.0.0",
				},
			},
			common.WorkerServiceName: {
				RPC: config.RPC{
					GRPCPort:        *b.instance.Spec.Services.Worker.Port,
					MembershipPort:  *b.instance.Spec.Services.Worker.MembershipPort,
					BindOnLocalHost: false,
					BindOnIP:        "0.0.0.0",
				},
			},
		},
		PublicClient: config.PublicClient{
			HostPort: fmt.Sprintf("%s:%d", b.instance.ChildResourceName("frontend"), *b.instance.Spec.Services.Frontend.Port),
		},
	}

	if b.instance.Spec.MTLS != nil && (b.instance.Spec.MTLS.InternodeEnabled() || b.instance.Spec.MTLS.FrontendEnabled()) {
		temporalCfg.Global.TLS = config.RootTLS{
			RefreshInterval:  1 * time.Hour,
			ExpirationChecks: config.CertExpirationValidation{},
		}

		internodeMTLS := b.instance.Spec.MTLS.Internode
		internodeIntermediateCAFilePath := path.Join(internodeMTLS.GetIntermediateCACertificateMountPath(), "tls.crt")
		serverCertFilePath := path.Join(internodeMTLS.GetCertificateMountPath(), "tls.crt")
		serverKeyFilePath := path.Join(internodeMTLS.GetCertificateMountPath(), "tls.key")
		internodeClientTLS := config.ClientTLS{
			ServerName:              internodeMTLS.ServerName(b.instance.ServerName()),
			DisableHostVerification: false,
			RootCAFiles:             []string{internodeIntermediateCAFilePath},
			ForceTLS:                true,
		}

		if b.instance.Spec.MTLS.InternodeEnabled() {
			temporalCfg.Global.TLS.Internode = config.GroupTLS{
				Client: internodeClientTLS,
				Server: config.ServerTLS{
					CertFile: serverCertFilePath,
					KeyFile:  serverKeyFilePath,
					ClientCAFiles: []string{
						internodeIntermediateCAFilePath,
					},
					RequireClientAuth: true,
				},
			}
		}

		if b.instance.Spec.MTLS.FrontendEnabled() {
			temporalCfg.Global.TLS.Frontend = config.GroupTLS{
				Server: config.ServerTLS{
					RequireClientAuth: true,
					// TODO(alexandrevilain): add frontend intermediate CA
					// ClientCAFiles: ,
				},
				PerHostOverrides: map[string]config.ServerTLS{},
			}
			temporalCfg.Global.TLS.SystemWorker = config.WorkerTLS{
				CertFile: serverCertFilePath,
				KeyFile:  serverKeyFilePath,
				Client:   internodeClientTLS,
			}
		}
	}

	result, err := yaml.Marshal(temporalCfg)
	if err != nil {
		return fmt.Errorf("failed marshaling temporal config: %w", err)
	}

	configMap.Data = map[string]string{
		"config_template.yaml": string(result),
	}

	if err := controllerutil.SetControllerReference(b.instance, configMap, b.scheme); err != nil {
		return fmt.Errorf("failed setting controller reference: %v", err)
	}

	return nil
}
