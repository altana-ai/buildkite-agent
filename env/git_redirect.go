package env

import "strings"

// gitRedirectingVars change which repository, objects or config git uses, or
// run a command of the caller's choosing, so a job could point the checkout
// at a commit that isn't on its branch.
var gitRedirectingVars = map[string]struct{}{
	"GIT_ALLOW_PROTOCOL":               {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_ASKPASS":                      {},
	"GIT_COMMON_DIR":                   {},
	"GIT_CONFIG":                       {},
	"GIT_CONFIG_COUNT":                 {},
	"GIT_CONFIG_GLOBAL":                {},
	"GIT_CONFIG_NOSYSTEM":              {},
	"GIT_CONFIG_PARAMETERS":            {},
	"GIT_CONFIG_SYSTEM":                {},
	"GIT_DIR":                          {},
	"GIT_EXEC_PATH":                    {},
	"GIT_GRAFT_FILE":                   {},
	"GIT_INDEX_FILE":                   {},
	"GIT_NAMESPACE":                    {},
	"GIT_NO_REPLACE_OBJECTS":           {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_PROTOCOL_FROM_USER":           {},
	"GIT_PROXY_COMMAND":                {},
	"GIT_REPLACE_REF_BASE":             {},
	"GIT_SHALLOW_FILE":                 {},
	"GIT_SSH":                          {},
	"GIT_SSH_COMMAND":                  {},
	"GIT_SSH_VARIANT":                  {},
	"GIT_SSL_CAINFO":                   {},
	"GIT_SSL_CAPATH":                   {},
	"GIT_SSL_NO_VERIFY":                {},
	"GIT_TEMPLATE_DIR":                 {},
	"GIT_WORK_TREE":                    {},
	"SSH_ASKPASS":                      {},
}

// IsGitRedirecting reports whether a job-supplied env var could make git
// fetch or check out something other than the commit the agent verified.
func IsGitRedirecting(name string) bool {
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return true
	}
	_, ok := gitRedirectingVars[name]
	return ok
}
