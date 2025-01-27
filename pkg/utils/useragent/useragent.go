/*
Copyright 2025.

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

package useragent

import (
	"fmt"
	"runtime"

	"sigs.k8s.io/lws/pkg/version"
)

func adjustCommit(c string) string {
	if len(c) == 0 {
		return "unknown"
	}
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func Default() string {
	return fmt.Sprintf("lws/%s (%s/%s) %s", version.GitVersion, runtime.GOOS, runtime.GOARCH, adjustCommit(version.GitCommit))
}
