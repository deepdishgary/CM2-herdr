package device

import "testing"

func TestGuardRefuses(t *testing.T) {
	refused := []string{
		"fs.write", "fs.writebin", "fs.delete", "fs.rm", "sys.bootloader", "sys.reset", "sys.reboot",
		"sys.factory", "sys.factory_reset", "sys.factoryreset", "sys.factoryErase",
		"FS.WRITE", " fs.write", "fs.write ", // case and spaces
		"keymap.write", "fs.erase", "flash.run", "dev.flashfw", "device.boot", "sys.bootmode", "x.reset", "lights.reset",
		"fs.mkdir", "fs.rename", "fs.unknown", "fs.format", "sys.dfu", "sys.ota", "cfg.save", "cfg.persist",
		"", "a b", "a;b", "fs.read\x00",
		"cfg.set", "keymap.apply", "nvs.get", "sys.restart", "bt.unpair", "fs.unlink", "fs.WRITE",
		"fs.read\n", "fs.read\tfs.write", "\u0066s.write", "fs.write\u200b", "fs.\u0077rite",
	}
	for _, m := range refused {
		if r, why := GuardMethod(m); !r || why == "" {
			t.Errorf("%q: not refused", m)
		}
	}
}

func TestGuardAllows(t *testing.T) {
	allowed := []string{
		"v.oai.thstatus", "v.oai.rgbcfg", "lights.preview", "V.OAI.THSTATUS",
		"device.status", "sys.version", "fs.read", "fs.list", "fs.stat",
		"v.oai.hid", "v.oai.rad",
	}
	for _, m := range allowed {
		if r, why := GuardMethod(m); r {
			t.Errorf("%q refused: %s", m, why)
		}
	}
}

func TestGuardRulesPrintable(t *testing.T) {
	rules := GuardRules()
	if len(rules) < 5 {
		t.Fatal("rules too short")
	}
}
