package gerritchecks

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed bridge-checks.js
var pluginJS string

// Plugin returns the Gerrit UI plugin (install as plugins/bridge-checks.js)
// that fetches runs from the bridge at bridgeURL for project.
func Plugin(bridgeURL, project string) []byte {
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return strings.Trim(string(b), `"`)
	}
	return []byte(strings.NewReplacer("__BRIDGE_URL__", quote(strings.TrimRight(bridgeURL, "/")),
		"__PROJECT__", quote(project)).Replace(pluginJS))
}
