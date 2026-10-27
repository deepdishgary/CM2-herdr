package painter

import (
	"context"
	"encoding/json"
	"fmt"
)

// Caller is the part of device.Client that DeviceLights needs.
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// DeviceLights sends threads to the pad with v.oai.thstatus.
type DeviceLights struct{ c Caller }

// NewDeviceLights returns Lights that call c, normally a *device.Client.
func NewDeviceLights(c Caller) *DeviceLights { return &DeviceLights{c: c} }

// wireThread is one entry of the v.oai.thstatus array.
type wireThread struct {
	ID int     `json:"id"`
	C  int     `json:"c"`
	B  float64 `json:"b"`
	E  int     `json:"e"`
	S  float64 `json:"s,omitempty"`
}

// SetThreads sends one v.oai.thstatus call with a bare array as params.
// An empty list sends nothing.
func (d *DeviceLights) SetThreads(ctx context.Context, threads []Thread) error {
	if len(threads) == 0 {
		return nil
	}
	arr := make([]wireThread, len(threads))
	for i, t := range threads {
		arr[i] = wireThread{ID: t.ID, C: t.Color, B: t.Brightness, E: t.Effect, S: t.Speed}
	}
	res, err := d.c.Call(ctx, "v.oai.thstatus", arr)
	if err != nil {
		return err
	}
	// The pad always answers {"ok":1}. Check only an explicit other value.
	var r struct {
		OK *int `json:"ok"`
	}
	if json.Unmarshal(res, &r) == nil && r.OK != nil && *r.OK != 1 {
		return fmt.Errorf("painter: v.oai.thstatus replied ok=%d", *r.OK)
	}
	return nil
}
