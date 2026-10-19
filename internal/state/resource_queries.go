package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResourceReader is an optional projection API. Results are values owned by the
// caller, from a committed revision serialized with Update. Errors must fail closed;
// callers must not replace a failed database read with a stale snapshot.
type ResourceReader interface {
	SiteByID(string) (Site, bool, error)
	ServiceByID(string) (Service, bool, error)
	ServiceByDomain(string) (Service, bool, error)
}

func (s *Store) SiteByID(id string) (Site, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Site{}, false, fmt.Errorf("state store is closed")
	}
	if b, ok := s.disk.(*sqliteBackend); ok {
		return b.siteByID(id)
	}
	for _, v := range s.state.Sites {
		if v.ID == id {
			return v, true, nil
		}
	}
	return Site{}, false, nil
}
func (s *Store) ServiceByID(id string) (Service, bool, error) {
	return s.readService(id, false)
}
func (s *Store) ServiceByDomain(domain string) (Service, bool, error) {
	return s.readService(domain, true)
}
func (s *Store) readService(key string, domain bool) (Service, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Service{}, false, fmt.Errorf("state store is closed")
	}
	if key == "" {
		return Service{}, false, nil
	}
	if b, ok := s.disk.(*sqliteBackend); ok {
		return b.serviceByKey(key, domain)
	}
	for _, v := range s.state.Services {
		if !domain && v.ID == key || domain && v.CustomDomain == key {
			return v, true, nil
		}
	}
	return Service{}, false, nil
}
func (b *sqliteBackend) siteByID(id string) (Site, bool, error) {
	var v Site
	var created string
	err := b.db.QueryRow("SELECT id,name,token_hash,created_at,organization_id,legacy_disabled FROM sites WHERE id=?", id).Scan(&v.ID, &v.Name, &v.TokenHash, &created, &v.OrganizationID, &v.LegacyDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, false, nil
	}
	if err != nil {
		return Site{}, false, err
	}
	v.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Site{}, false, err
	}
	return v, true, nil
}
func (b *sqliteBackend) serviceByKey(key string, domain bool) (Service, bool, error) {
	where := "id=?"
	if domain {
		where = "custom_domain=? AND custom_domain!=''"
	}
	var v Service
	err := b.db.QueryRow("SELECT id,name,site_id,url,organization_id,custom_domain,health_path,health_method,health_expected_status,connection_mode FROM services WHERE "+where, key).Scan(&v.ID, &v.Name, &v.SiteID, &v.URL, &v.OrganizationID, &v.CustomDomain, &v.HealthPath, &v.HealthMethod, &v.HealthExpectedStatus, &v.ConnectionMode)
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, false, nil
	}
	if err != nil {
		return Service{}, false, err
	}
	return v, true, nil
}
