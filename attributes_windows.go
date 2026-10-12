//go:build windows

package bt

import "golang.org/x/sys/windows/registry"

// collectMachineInfo gathers Windows machine metadata from the registry —
// native API calls, no subprocesses (wmic is removed from Windows 11 24H2
// and reg.exe is unnecessary).
func collectMachineInfo(attrs map[string]interface{}, d diag) {
	if v := regString(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString", d); v != "" {
		attrs["cpu.brand"] = v
	}
	// DisplayVersion exists on 20H2+; ReleaseId covers 1511..2004 (and is
	// frozen at "2009" afterwards, hence the ordering).
	version := regString(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "DisplayVersion", d)
	if version == "" {
		version = regString(registry.LOCAL_MACHINE,
			`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "ReleaseId", d)
	}
	if version != "" {
		attrs["uname.version"] = version
	}
}

// collectMachineGUID reads the stable machine identifier (opt-in via
// Config.SendMachineID) from the registry.
func collectMachineGUID(d diag) string {
	return regString(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, "MachineGuid", d)
}

func regString(root registry.Key, path, name string, d diag) string {
	// WOW64_64KEY: read the 64-bit registry view so 32-bit builds see the
	// same MachineGuid/CPU keys as native ones.
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		d.logf("machine attribute registry open %q failed: %v", path, err)
		return ""
	}
	defer key.Close()
	value, _, err := key.GetStringValue(name)
	if err != nil {
		d.logf("machine attribute registry read %q\\%s failed: %v", path, name, err)
		return ""
	}
	return value
}
