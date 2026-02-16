/*
Copyright 2026.

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

package impersonate

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewClient returns a controller-runtime client that impersonates the given
// ServiceAccount for all API requests. The base config is copied (never mutated),
// and the shared scheme/mapper avoid re-discovery on each call.
func NewClient(baseCfg *rest.Config, scheme *runtime.Scheme, mapper meta.RESTMapper,
	namespace, saName string) (client.Client, error) {

	cfg := rest.CopyConfig(baseCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
		Groups: []string{
			"system:serviceaccounts",
			fmt.Sprintf("system:serviceaccounts:%s", namespace),
		},
	}

	return client.New(cfg, client.Options{
		Scheme: scheme,
		Mapper: mapper,
	})
}
