package blobstore

import (
	"encoding/json"

	"github.com/yolocs/open-artifact/pkg/core"
)

// encodeMeta serializes a core.Meta envelope to its on-bucket JSON form.
func encodeMeta(m core.Meta) ([]byte, error) {
	return json.Marshal(m)
}

// decodeMeta parses an on-bucket .meta object back into a core.Meta envelope.
func decodeMeta(b []byte) (core.Meta, error) {
	var m core.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return core.Meta{}, err
	}
	return m, nil
}
