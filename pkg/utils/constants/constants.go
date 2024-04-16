/*
Copyright 2023.

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

package constants

const (
	// RollingUpdateFailedReason
	// Event reason used when a LeaderWorkerSet rolling update fails.
	// The event uses the error(s) as the reason.
	RollingUpdateFailedReason = "RollingUpdateFailed"
	// SSALeaderWorkerSetFailedReason
	// Event reason used when a LeaderWorkerSet server side apply fails.
	// The event uses the error(s) as the reason.
	SSALeaderWorkerSetFailedReason = "ServerSideApplyLeaderWorkerSetFailed"
	// HeadlessServiceCreationFailedReason
	// Event reason used when a Headless Service creation fails.
	// The event uses the error(s) as the reason.
	HeadlessServiceCreationFailedReason = "HeadlessServiceCreationFailed"
	// UpdateStatusFailedReason
	// Event reason used when a LeaderWorkerSet update fails.
	// The event uses the error(s) as the reason.
	UpdateStatusFailedReason = "UpdateStatusFailed"
)
