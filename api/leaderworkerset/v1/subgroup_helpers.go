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

package v1

import "encoding/json"

func EncodeSubGroupPlacement(placement []SubGroupPlacement) (string, error) {
	if len(placement) == 0 {
		return "", nil
	}
	data, err := json.Marshal(placement)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func DecodeSubGroupPlacement(value string) ([]SubGroupPlacement, error) {
	if value == "" {
		return nil, nil
	}
	var placement []SubGroupPlacement
	if err := json.Unmarshal([]byte(value), &placement); err != nil {
		return nil, err
	}
	return placement, nil
}

func EncodeSubGroupMembers(members []int32) (string, error) {
	if len(members) == 0 {
		return "", nil
	}
	data, err := json.Marshal(members)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func DecodeSubGroupMembers(value string) ([]int32, error) {
	if value == "" {
		return nil, nil
	}
	var members []int32
	if err := json.Unmarshal([]byte(value), &members); err != nil {
		return nil, err
	}
	return members, nil
}
