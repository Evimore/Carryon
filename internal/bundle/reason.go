package bundle

// Reason says why grouping put an item in its bundle.
type Reason struct {
	Step string `json:"step"` // which step put it there: one of the Step* constants
	Text string `json:"text"` // in plain words, to follow "because"
}

// The grouping steps, in order of precedence. See docs/design.md, "Bundles and when-rules".
const (
	StepDiskLayout     = "disk-layout"     // a file with disk IDs
	StepPut            = "put"             // a put line in bundles.conf
	StepAppName        = "app-name"        // named like a unit you made, an extra repository or AUR packages
	StepFingerprint    = "fingerprint"     // a PAM file using the fingerprint reader
	StepWellKnown      = "well-known"      // a well-known place
	StepEditedConfigs  = "edited-configs"  // a package's edited settings, with its service
	StepSettingsFolder = "settings-folder" // a settings folder named like a package
	StepAutostart      = "autostart"       // an autostart entry of a package
	StepExtraFolder    = "extra-folder"    // an added system folder named like a package
	StepHardware       = "hardware"        // kernel parameters and firmware
	StepFallback       = "fallback"        // the catch-all bundle of its kind
	StepUnitNeeds      = "unit-needs"      // something a unit needs, with the unit
)
