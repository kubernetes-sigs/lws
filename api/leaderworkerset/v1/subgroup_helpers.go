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
