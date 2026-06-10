package bmc

const (
	PowerOn      PowerAction = "on"
	PowerHardOff PowerAction = "off"
	PowerSoftOff PowerAction = "soft"
	PowerCycle   PowerAction = "cycle"
	PowerReset   PowerAction = "reset"
	PowerStatus  PowerAction = "status"

	PXE   BootDevice = "pxe"
	Disk  BootDevice = "disk"
	BIOS  BootDevice = "bios"
	CDROM BootDevice = "cdrom"
	Safe  BootDevice = "safe"

	// VirtualMediaCD represents a virtual CD-ROM.
	VirtualMediaCD VirtualMediaKind = "CD"
)

// BootDevice represents boot device of the Machine.
type BootDevice string

// VirtualMediaKind represents the kind of virtual media.
type VirtualMediaKind string

// PowerAction represents the power control operation on the baseboard management.
type PowerAction string

// OneTimeBootDeviceAction represents a single operation to set the machine's one-time boot device via the BMC.
// Deprecated. Will be removed in a future release. Use BootDeviceConfig instead.
type OneTimeBootDeviceAction struct {
	// Devices represents the boot devices, in order for setting one time boot.
	// Currently only the first device in the slice is used to set one time boot.
	Devices []BootDevice `json:"device"`

	// EFIBoot instructs the machine to use EFI boot.
	EFIBoot bool `json:"efiBoot,omitempty"`
}

// VirtualMediaAction represents a virtual media action.
type VirtualMediaAction struct {
	// mediaURL represents the URL of the image to be inserted into the virtual media, or empty to eject media.
	MediaURL string `json:"mediaURL,omitempty"`

	// Kind represents the kind of virtual media.
	Kind VirtualMediaKind `json:"kind"`
}

// BootDeviceConfig represents the configuration for setting a boot device.
type BootDeviceConfig struct {
	// Device is the name of the device to set as the first boot device.
	Device BootDevice `json:"device,omitempty"`

	// Persistent indicates whether the boot device should be set persistently as the first boot device.
	Persistent bool `json:"persistent,omitempty"`

	// EFIBoot indicates whether the boot device should be set to efiboot mode.
	EFIBoot bool `json:"efiBoot,omitempty"`
}

// PowerCapAction sets or clears the chassis power limit (in watts).
// The watt value is passed to the bmclib SetPowerCap client method; the
// provider translates it to the controller-appropriate Redfish payload.
type PowerCapAction struct {
	// LimitWatts is the power cap to apply, in watts. Ignored when Disable is true.
	// +optional
	LimitWatts *int64 `json:"limitWatts,omitempty"`

	// Disable removes any active power cap. When true, LimitWatts is ignored and
	// the cap is cleared (bmclib SetPowerCap is called with a nil limit).
	// +optional
	Disable bool `json:"disable,omitempty"`
}

// SecureBootAction reads and sets UEFI Secure Boot via the bmclib
// GetSecureBoot/SetSecureBoot client methods.
type SecureBootAction struct {
	// Enable sets UEFI Secure Boot enabled (true) or disabled (false).
	Enable bool `json:"enable"`
}

// InventoryAction triggers a hardware-inventory read via the bmclib Inventory
// client method. The summary (vendor/model/component counts) is recorded in the
// Task status Result map.
type InventoryAction struct{}

// FirmwareAction installs firmware via the bmclib FirmwareInstall client method
// and polls the resulting task to a terminal state. The provider owns the XCC
// push protocol (claim/push/poll/release); rufio never GETs the TaskMonitor URI.
type FirmwareAction struct {
	// ImageURL is the URL the controller fetches the firmware image from.
	ImageURL string `json:"imageURL"`

	// Component is the firmware component target (empty means provider auto-detect).
	// +optional
	Component string `json:"component,omitempty"`

	// ApplyTime is the Redfish OperationApplyTime carried in the multipart push
	// UpdateParameters (e.g. OnReset, Immediate). It is distinct from the BIOS
	// settings apply-time annotation; leave empty for the provider default.
	// +optional
	ApplyTime string `json:"applyTime,omitempty"`

	// Force installs even when the running version matches the image.
	// +optional
	Force bool `json:"force,omitempty"`
}

func (b BootDevice) String() string {
	return string(b)
}

func (v VirtualMediaKind) String() string {
	return string(v)
}

func (p PowerAction) String() string {
	return string(p)
}
