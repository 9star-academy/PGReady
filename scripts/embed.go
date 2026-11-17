package scripts

import _ "embed"

// Installer is also the standalone installer attached to each release.
//
//go:embed install.sh
var Installer string
