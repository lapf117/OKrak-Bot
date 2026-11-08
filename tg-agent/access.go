package main

// Roles and provider lookups. Every check reads a fresh snapshot so that
// permission changes apply immediately, even to running tasks.

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func isOwner(uid int64) bool { c := configSnapshot(); return c.OwnerID > 0 && uid == c.OwnerID }

func isAdmin(uid int64) bool {
	c := configSnapshot()
	return c.OwnerID > 0 && uid == c.OwnerID || containsID(c.Admins, uid)
}

func isFriend(uid int64) bool { return containsID(configSnapshot().Friends, uid) }
func isBanned(uid int64) bool { return containsID(configSnapshot().Banned, uid) }

func allowed(uid int64) bool {
	c := configSnapshot()
	return !containsID(c.Banned, uid) && (c.OwnerID > 0 && uid == c.OwnerID || containsID(c.Admins, uid) || containsID(c.Friends, uid))
}

func roleKey(uid int64) string {
	switch {
	case isOwner(uid):
		return "role.owner"
	case isAdmin(uid):
		return "role.admin"
	case isFriend(uid):
		return "role.friend"
	}
	return "role.guest"
}

func roleName(uid int64) string                  { return tr(langZH, roleKey(uid)) }
func roleNameLang(uid int64, lang string) string { return tr(lang, roleKey(uid)) }

func toggleID(list []int64, id int64, add bool) []int64 {
	if add {
		if containsID(list, id) {
			return list
		}
		return append(list, id)
	}
	var keep []int64
	for _, x := range list {
		if x != id {
			keep = append(keep, x)
		}
	}
	return keep
}

func currentProvider() *Provider { return providerFor(configSnapshot()) }

func providerFor(c Config) *Provider {
	for _, p := range c.Provider {
		if p.Name == c.ProviderName {
			q := p
			return &q
		}
	}
	if len(c.Provider) > 0 {
		p := c.Provider[0]
		return &p
	}
	return nil
}

func setProviderIn(c *Config, p Provider) {
	for i := range c.Provider {
		if c.Provider[i].Name == p.Name {
			c.Provider[i] = p
			return
		}
	}
	c.Provider = append(c.Provider, p)
}
