package scope

import (
	"fmt"
	"strconv"
	"strings"

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
	// RuntimeRelabeled is non-empty when the object is the registered one but
	// its runtime volume label changed across a boot. It is a fact, not an
	// approval: the vault binding and the scope row still carry the old label,
	// and only an explicit `scope rebind` may establish the new generation.
	RuntimeRelabeled string
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

	// `dev-<number>` is a runtime device number, not a stable volume UUID. Both
	// platform files currently derive VolumeID from stat.st_dev (see
	// internal/fsops/platform_*.go), and macOS renumbers devices across boots.
	// A boot change therefore cannot prove a volume swap from those two facts.
	// A value in any other shape is treated as a genuinely persistent label and
	// stays comparable across boots.
	newBoot := stored.Runtime.BootOrRunID != "" && current.Runtime.BootOrRunID != "" &&
		stored.Runtime.BootOrRunID != current.Runtime.BootOrRunID
	relabeled := ""

	if stored.Persistent.Inode != current.Persistent.Inode {
		return changed("the root inode changed from %d to %d",
			stored.Persistent.Inode, current.Persistent.Inode)
	}
	if stored.Persistent.VolumeID != "" && current.Persistent.VolumeID != "" &&
		stored.Persistent.VolumeID != current.Persistent.VolumeID {
		if !newBoot || !LegacyVolumeLabel(stored.Persistent.VolumeID) ||
			!LegacyVolumeLabel(current.Persistent.VolumeID) {
			return changed("the root volume changed from %s to %s",
				stored.Persistent.VolumeID, current.Persistent.VolumeID)
		}
		relabeled = fmt.Sprintf("the runtime volume label changed from %s to %s across a new boot",
			stored.Persistent.VolumeID, current.Persistent.VolumeID)
	}
	if stored.Runtime.Device != 0 && current.Runtime.Device != 0 &&
		stored.Runtime.Device != current.Runtime.Device {
		// Inside one boot a different device *is* a different way of reaching
		// this path, and CompareMountBinding cannot see it on macOS because
		// mountIDFor returns nil there. Keep that detection. Across boots the
		// number is not comparable at all.
		if !newBoot {
			return changed("the root device changed from %d to %d",
				stored.Runtime.Device, current.Runtime.Device)
		}
		if relabeled == "" {
			relabeled = fmt.Sprintf("the runtime device number changed from %d to %d across a new boot",
				stored.Runtime.Device, current.Runtime.Device)
		}
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
	return IdentityVerdict{RuntimeRelabeled: relabeled}
}

// known reports whether a filesystem name was actually established.
func known(name string) bool { return name != "" && name != "unknown" }

// LegacyVolumeLabel reports whether a stored volume id has the `dev-<number>`
// shape this build derives from the runtime device number. Such a value is
// runtime-scoped, not a stable volume UUID. Any other shape is compared as a
// persistent fact; that is how a future platform that can supply a real volume
// UUID becomes stricter without changing this comparison again.
func LegacyVolumeLabel(v string) bool {
	if !strings.HasPrefix(v, "dev-") || len(v) <= 4 {
		return false
	}
	_, err := strconv.ParseUint(v[4:], 10, 64)
	return err == nil
}

// LegacyDevice returns the device number encoded by a legacy volume label.
func LegacyDevice(v string) (uint64, bool) {
	if !LegacyVolumeLabel(v) {
		return 0, false
	}
	n, err := strconv.ParseUint(v[4:], 10, 64)
	return n, err == nil
}

// RootBootChanged reports whether the two identities were observed in different
// boot sessions. Both sides must name a boot: an unavailable boot id is not
// evidence of a restart.
func RootBootChanged(stored, current domain.RootIdentity) bool {
	return stored.Runtime.BootOrRunID != "" && current.Runtime.BootOrRunID != "" &&
		stored.Runtime.BootOrRunID != current.Runtime.BootOrRunID
}

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
