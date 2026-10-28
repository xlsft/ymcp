package workspace

import "strings"

// Identity is the git author an agent commits as.
type Identity struct{ Name, Email string }

// AgentIdentity is the commit identity of one run: name, and userEmail
// with tag after a "+" in its local part, so dev@xlsft.ru becomes
// dev+<tag>@xlsft.ru; an existing +tag is replaced. The tag is reduced to
// lowercase letters, digits and underscores, and an empty tag leaves the
// address without one. Characters git refuses in a name are dropped; an
// empty name is "Agent". An address without "@" gives an empty Email.
func AgentIdentity(userEmail, name, tag string) Identity {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name))
	if name == "" {
		name = "Agent"
	}
	id := Identity{Name: name}
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
	if tag = tagSlug(tag); tag != "" {
		local += "+" + tag
	}
	id.Email = local + "@" + domain
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
	return b.String()
}
