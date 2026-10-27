package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Layout is the part of keymap.json that cm2d owns: the keys and encoders of
// one layer of the active profile. Everything else in the file is kept.
// A null cell means "keep the code the pad has there now".
type Layout struct {
	Layer    int         `json:"layer"`              // layer id to change
	Keymap   [][]*string `json:"keymap,omitempty"`   // rows, same shape as the pad's layout
	Encoders [][]*string `json:"encoders,omitempty"` // [cw, ccw, press] per encoder
}

// LayoutFile holds the layouts for one or more layers.
type LayoutFile struct {
	Layers []Layout `json:"layers"`
}

// ApplyLayouts applies each layout in turn and returns the combined changes.
func ApplyLayouts(cur []byte, f LayoutFile) ([]byte, []KeyChange, error) {
	if len(f.Layers) == 0 {
		return nil, nil, errors.New("layout file has no layers")
	}
	var all []KeyChange
	doc := cur
	for _, l := range f.Layers {
		next, ch, err := ApplyLayout(doc, l)
		if err != nil {
			return nil, nil, fmt.Errorf("layer %d: %w", l.Layer, err)
		}
		for i := range ch {
			ch[i].Where = fmt.Sprintf("layer %d %s", l.Layer, ch[i].Where)
		}
		all = append(all, ch...)
		doc = next
	}
	return doc, all, nil
}

// KeyChange is one key whose code changes.
type KeyChange struct {
	Where    string // e.g. "keymap[1][2]" or "encoders[0][2]"
	Old, New string
}

// ApplyLayout returns cur (a keymap.json document) with the layout written
// into the target layer of the active profile, plus the list of changed keys.
// It refuses a layout whose shape differs from the pad's, so a wrong file
// cannot shift keys into the wrong places.
func ApplyLayout(cur []byte, l Layout) ([]byte, []KeyChange, error) {
	doc, err := decodeKeymap(cur)
	if err != nil {
		return nil, nil, err
	}
	layer, err := findLayer(doc, l.Layer)
	if err != nil {
		return nil, nil, err
	}
	layout, ok := layer["layout"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("layer %d has no layout object", l.Layer)
	}
	var changes []KeyChange
	if l.Keymap == nil && l.Encoders == nil {
		return nil, nil, errors.New("layout has neither keymap nor encoders")
	}
	if l.Keymap != nil {
		ch, err := replaceGrid(layout, "keymap", l.Keymap)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, ch...)
	}
	if l.Encoders != nil {
		ch, err := replaceGrid(layout, "encoders", l.Encoders)
		if err != nil {
			return nil, nil, err
		}
		changes = append(changes, ch...)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	return out, changes, nil
}

// SameKeymap reports whether two keymap.json documents hold the same data,
// ignoring key order and whitespace.
func SameKeymap(a, b []byte) bool {
	da, err1 := decodeKeymap(a)
	db, err2 := decodeKeymap(b)
	return err1 == nil && err2 == nil && reflect.DeepEqual(da, db)
}

func decodeKeymap(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep numbers such as 0.1875 exactly as written
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("keymap.json is not a JSON object: %w", err)
	}
	return doc, nil
}

// findLayer returns the layer with the given id in the active profile.
func findLayer(doc map[string]any, layerID int) (map[string]any, error) {
	profiles, ok := doc["profiles"].([]any)
	if !ok || len(profiles) == 0 {
		return nil, errors.New("keymap.json has no profiles")
	}
	active, err := intOf(doc["activeProfileId"])
	if err != nil {
		return nil, fmt.Errorf("activeProfileId: %w", err)
	}
	var profile map[string]any
	for _, p := range profiles {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if id, err := intOf(m["id"]); err == nil && id == active {
			profile = m
			break
		}
	}
	if profile == nil {
		return nil, fmt.Errorf("active profile %d not found", active)
	}
	layers, ok := profile["layers"].([]any)
	if !ok {
		return nil, fmt.Errorf("profile %d has no layers", active)
	}
	for _, l := range layers {
		m, ok := l.(map[string]any)
		if !ok {
			continue
		}
		if id, err := intOf(m["id"]); err == nil && id == layerID {
			return m, nil
		}
	}
	return nil, fmt.Errorf("layer id %d not found in profile %d", layerID, active)
}

func intOf(v any) (int, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, fmt.Errorf("not a number: %v", v)
	}
	i, err := n.Int64()
	return int(i), err
}

// replaceGrid swaps layout[key] (rows of key codes) for want, after checking
// that the row count and row lengths match.
func replaceGrid(layout map[string]any, key string, want [][]*string) ([]KeyChange, error) {
	rows, ok := layout[key].([]any)
	if !ok {
		return nil, fmt.Errorf("layer has no %s", key)
	}
	if len(rows) != len(want) {
		return nil, fmt.Errorf("%s: the pad has %d rows, the layout file has %d", key, len(rows), len(want))
	}
	var changes []KeyChange
	newRows := make([]any, len(want))
	for i, r := range rows {
		cells, ok := r.([]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] is not a list", key, i)
		}
		if len(cells) != len(want[i]) {
			return nil, fmt.Errorf("%s[%d]: the pad has %d keys, the layout file has %d", key, i, len(cells), len(want[i]))
		}
		row := make([]any, len(want[i]))
		for j, c := range cells {
			if want[i][j] == nil {
				row[j] = c // keep the pad's code
				continue
			}
			w := *want[i][j]
			old, _ := c.(string)
			if w == "" {
				return nil, fmt.Errorf("%s[%d][%d] is empty in the layout file (use KC_NONE, or null to keep)", key, i, j)
			}
			if old != w {
				changes = append(changes, KeyChange{Where: fmt.Sprintf("%s[%d][%d]", key, i, j), Old: old, New: w})
			}
			row[j] = w
		}
		newRows[i] = row
	}
	layout[key] = newRows
	return changes, nil
}
