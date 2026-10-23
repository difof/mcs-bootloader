// Package assets contains the installation resources shipped with every binary.
// Files are embedded from their maintained source locations, never generated
// copies. Only operational helpers are included; tests and private .env are not.
package assets

import "embed"

// Files is the immutable runtime bundle used by internal/operator.
//
//go:embed .env.example scripts/env_config.py scripts/manage.py scripts/install.py scripts/service_config.py scripts/remove.py deploy/mcs-bootloader.service deploy/mcs-bootloader-backend.service deploy/journald@mcs-bootloader.conf
var Files embed.FS

// License is available without Python, configuration, or an installed service.
//
//go:embed LICENSE
var License string

const LicenseID = "AGPL-3.0-only"
