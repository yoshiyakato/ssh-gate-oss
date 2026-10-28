//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// On Windows the 0600 / 0700 modes ssh-gete sets do not limit who can read a
// file: it inherits the ACL of its parent, which under %APPDATA% may grant
// other principals access. The config directory, the database (with its
// -wal, -shm, lock, .bak-v* copies) and the *.migrated files hold the shared
// MCP token and the approval-bypass grants, so each gets a protected DACL
// granting only the current user and SYSTEM (N41). A directory's entries are
// inheritable, so files SQLite creates in it later get the same.
func init() {
	restrictACL = setOwnerOnlyDACL
}

func setOwnerOnlyDACL(path string, isDir bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("SYSTEM sid: %w", err)
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if isDir {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	grant := func(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  trusteeType,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		grant(user.User.Sid, windows.TRUSTEE_IS_USER),
		grant(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	if err != nil {
		return fmt.Errorf("build dacl: %w", err)
	}
	// PROTECTED: the parent's inheritable entries no longer apply.
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
