package gerritchecks

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
)

//go:embed bridge-checks.js
var pluginJS string

// Plugin returns the Gerrit UI plugin (install as plugins/bridge-checks.js)
// that fetches runs from the bridge at bridgeURL for project. rerun says
// whether its Rerun actions post "retest", which Gerrit's CI takes.
func Plugin(bridgeURL, project string, rerun bool) []byte {
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return strings.Trim(string(b), `"`)
	}
	return []byte(strings.NewReplacer("__BRIDGE_URL__", quote(strings.TrimRight(bridgeURL, "/")),
		"__PROJECT__", quote(project), "__RERUN__", strconv.FormatBool(rerun)).Replace(pluginJS))
}
