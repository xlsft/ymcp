package workspace

import (
	"fmt"
	"strings"
)

// Identity is the git author an agent commits as.
type Identity struct{ Name, Email string }

// AgentIdentity derives the commit identity of one run from the user's own
// address. The plus tag goes after the local part, so dev@xlsft.ru becomes
// dev+agent_<pipeline>_<run>@xlsft.ru; an existing +tag is replaced. The
// pipeline name is reduced to lowercase letters, digits and underscores. The
// name is "<agentName> · <pipeline> #<run>". An address without "@" gives an
// empty Email.
func AgentIdentity(userEmail, pipeline string, run int, agentName string) Identity {
	if strings.TrimSpace(agentName) == "" {
		agentName = "Agent"
	}
	id := Identity{Name: fmt.Sprintf("%s · %s #%d", strings.TrimSpace(agentName), pipeline, run)}
	userEmail = strings.TrimSpace(userEmail)
	at := strings.LastIndex(userEmail, "@")
	if at <= 0 || at == len(userEmail)-1 {
		return id
	}
	local, domain := userEmail[:at], userEmail[at+1:]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	if local == "" {
		return id
	}
	id.Email = fmt.Sprintf("%s+agent_%s_%d@%s", local, tagSlug(pipeline), run, domain)
	return id
}

// tagSlug keeps lowercase ASCII letters and digits; every run of anything else
// becomes one underscore.
func tagSlug(s string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)
		default:
			pendingSep = true
		}
	}
	if b.Len() == 0 {
		return "pipeline"
	}
	return b.String()
}
