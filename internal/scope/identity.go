package scope

import (
	"fmt"

	"dustkeeper/internal/domain"
)

// IdentityVerdict is the result of comparing a stored root identity with the one
// just observed.
//
// R02 separates two questions that the old code answered with a single "does the
// inode match": "is this still the object that was authorised" and "is the
// evidence strong enough to say so". Only the first is a reason to refuse
// everything; the second is a reason to stop acting automatically.
type IdentityVerdict struct {
	// Changed is non-empty when the current object cannot be the registered one.
	// The scope is held and no action of any kind may run.
	Changed string
	// Weak is non-empty when nothing proved a change but the surviving evidence
	// cannot distinguish this object from a different one that reuses the same
	// inode on a different volume. The scope stays usable for observing and
	// planning, and automatic permanent deletion is disabled until the operator
	// re-registers or confirms it.
	Weak string
}

// OK reports whether neither a change nor thin evidence was found.
func (v IdentityVerdict) OK() bool { return v.Changed == "" && v.Weak == "" }

// CompareRootIdentity compares a stored identity with a freshly observed one.
//
// Every field that both sides carry is compared. The previous implementation
// skipped the owner check entirely when the stored owner was 0 (root), and never
// compared the birth time or generation it had already collected - which is the
// exact shape of bug R02 describes: the evidence existed and was not used.
func CompareRootIdentity(stored, current domain.RootIdentity) IdentityVerdict {
	changed := func(format string, args ...any) IdentityVerdict {
		return IdentityVerdict{Changed: fmt.Sprintf(format, args...)}
	}

	if stored.Persistent.Inode != current.Persistent.Inode {
		return changed("the root inode changed from %d to %d",
			stored.Persistent.Inode, current.Persistent.Inode)
	}
	if stored.Persistent.VolumeID != "" && current.Persistent.VolumeID != "" &&
		stored.Persistent.VolumeID != current.Persistent.VolumeID {
		return changed("the root volume changed from %s to %s",
			stored.Persistent.VolumeID, current.Persistent.VolumeID)
	}
	if stored.Runtime.Device != 0 && current.Runtime.Device != 0 &&
		stored.Runtime.Device != current.Runtime.Device {
		return changed("the root device changed from %d to %d",
			stored.Runtime.Device, current.Runtime.Device)
	}
	// Exact comparison, including uid 0. Treating a stored zero as "unset" is
	// what let a re-chowned directory pass as the same object.
	if stored.Persistent.OwnerUID != current.Persistent.OwnerUID {
		return changed("the root owner changed from uid %d to uid %d",
			stored.Persistent.OwnerUID, current.Persistent.OwnerUID)
	}
	if known(stored.Persistent.FileSystem) && known(current.Persistent.FileSystem) &&
		stored.Persistent.FileSystem != current.Persistent.FileSystem {
		return changed("the root filesystem changed from %s to %s",
			stored.Persistent.FileSystem, current.Persistent.FileSystem)
	}

	// Two independent pieces of persistent evidence, compared only when both
	// sides carry them. A field that one side cannot read is not a match.
	strong := 0
	if s, c := stored.Persistent.BirthTimeNS, current.Persistent.BirthTimeNS; s != nil && c != nil {
		if *s != *c {
			return changed("the root creation time changed from %d to %d", *s, *c)
		}
		strong++
	}
	if s, c := stored.Persistent.Generation, current.Persistent.Generation; s != nil && c != nil {
		if *s != *c {
			return changed("the root generation changed from %d to %d", *s, *c)
		}
		strong++
	}

	if strong == 0 {
		return IdentityVerdict{Weak: "the registered root identity carries no creation time or " +
			"generation, so a volume swap or an inode reuse cannot be told apart from the original"}
	}
	return IdentityVerdict{}
}

// known reports whether a filesystem name was actually established.
func known(name string) bool { return name != "" && name != "unknown" }

// CompareMountBinding compares the runtime mount identity recorded for this scope
// with the one observed now.
//
// Mount IDs are only unique within a boot, so the comparison is only meaningful
// when both sides name the same boot. A reboot is not a change: it is the run
// that re-establishes the binding. A different mount inside one boot is a change,
// because the same path now resolves through something else - a moved directory,
// a bind mount, or a remounted volume.
func CompareMountBinding(stored, current domain.RuntimeMountIdentity) IdentityVerdict {
	if stored.BootOrRunID == "" || current.BootOrRunID == "" {
		return IdentityVerdict{}
	}
	if stored.BootOrRunID != current.BootOrRunID {
		// New boot: the previous mount IDs are not comparable. This is the normal
		// restart path and must never cost a legitimate authorisation.
		return IdentityVerdict{}
	}
	if stored.MountID != nil && current.MountID != nil && *stored.MountID != *current.MountID {
		return IdentityVerdict{Changed: fmt.Sprintf(
			"the root is reached through mount %d now, but was reachable through mount %d earlier in this boot",
			*current.MountID, *stored.MountID)}
	}
	return IdentityVerdict{}
}
