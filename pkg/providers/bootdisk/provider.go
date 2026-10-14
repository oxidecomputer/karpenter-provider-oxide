package bootdisk

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
	ImageID      string
	SizeBytes    int64
	Hash         string
	NodeClass    string
	NodeClassUID types.UID
}

type Acquired struct {
	RecordName string
	DiskName   string
	DiskID     string
	ClaimUID   types.UID
}

type Provider struct {
	kubeClient client.Client
	reader     client.Reader
	disks      *diskprovider.Provider
	clusterUID types.UID
}

func NewProvider(
	kubeClient client.Client,
	reader client.Reader,
	disks *diskprovider.Provider,
	clusterUID types.UID,
) *Provider {
	return &Provider{
		kubeClient: kubeClient,
		reader:     reader,
		disks:      disks,
		clusterUID: clusterUID,
	}
}

func ConfigFor(nodeClass *v1alpha1.OxideNodeClass) (Config, bool) {
	if len(nodeClass.Status.Images) == 0 || nodeClass.Spec.Storage.Boot.Size.Value() <= 0 {
		return Config{}, false
	}
	imageID := nodeClass.Status.Images[0].ID
	size := nodeClass.Spec.Storage.Boot.Size.Value()
	sum := sha256.Sum256([]byte(imageID + "\x00" + strconv.FormatInt(size, 10)))
	hash := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:12]
	return Config{
		ImageID:      imageID,
		SizeBytes:    size,
		Hash:         hash,
		NodeClass:    nodeClass.Name,
		NodeClassUID: nodeClass.UID,
	}, true
}

func NewRecord(config Config, pool *v1alpha1.OxideBootDiskPool) *v1alpha1.OxideBootDisk {
	name := "kbd-" + config.Hash[:6] + "-" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	return &v1alpha1.OxideBootDisk{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Finalizers: []string{v1alpha1.BootDiskFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "OxideBootDiskPool",
				Name:       pool.Name,
				UID:        pool.UID,
			}},
		},
		Spec: v1alpha1.OxideBootDiskSpec{
			PoolRef: v1alpha1.BootDiskOwnerRef{Name: pool.Name, UID: pool.UID},
			NodeClassRef: v1alpha1.BootDiskOwnerRef{
				Name: config.NodeClass,
				UID:  config.NodeClassUID,
			},
			DiskName:   name,
			ImageID:    config.ImageID,
			SizeBytes:  config.SizeBytes,
			ConfigHash: config.Hash,
		},
		Status: v1alpha1.OxideBootDiskStatus{State: v1alpha1.BootDiskStatePending},
	}
}

func (p *Provider) Acquire(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
	nodeClaimName string,
	nodeClaimUID types.UID,
) (*Acquired, error) {
	list := &v1alpha1.OxideBootDiskList{}
	if err := p.reader.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		record := &list.Items[i]
		if record.DeletionTimestamp.IsZero() && !record.Spec.Retire &&
			record.Spec.ClaimRef != nil &&
			record.Spec.ClaimRef.Name == nodeClaimName &&
			record.Spec.ClaimRef.UID == nodeClaimUID {
			return acquired(record), nil
		}
	}

	config, ok := ConfigFor(nodeClass)
	if !ok {
		return nil, nil
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.OxideBootDisk) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	for i := range list.Items {
		record := &list.Items[i]
		if !available(record, nodeClass.UID, config.Hash) {
			continue
		}
		disk, err := p.View(ctx, record)
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				_ = p.Retire(ctx, record.Name)
				continue
			}
			return nil, err
		}
		if disk.State.State() != oxide.DiskStateStateDetached {
			_ = p.Retire(ctx, record.Name)
			continue
		}
		stored := record.DeepCopy()
		record.Spec.ClaimRef = &v1alpha1.BootDiskOwnerRef{Name: nodeClaimName, UID: nodeClaimUID}
		record.Spec.ClaimedAt = &metav1.Time{Time: time.Now()}
		if err := p.kubeClient.Patch(ctx, record, client.MergeFromWithOptions(
			stored,
			client.MergeFromWithOptimisticLock{},
		)); err != nil {
			continue
		}
		return acquired(record), nil
	}
	return nil, nil
}

func available(record *v1alpha1.OxideBootDisk, nodeClassUID types.UID, hash string) bool {
	return record.DeletionTimestamp.IsZero() &&
		!record.Spec.Retire &&
		record.Spec.ClaimRef == nil &&
		record.Spec.NodeClassRef.UID == nodeClassUID &&
		record.Spec.ConfigHash == hash &&
		record.Status.State == v1alpha1.BootDiskStateReady
}

func acquired(record *v1alpha1.OxideBootDisk) *Acquired {
	return &Acquired{
		RecordName: record.Name,
		DiskName:   record.Spec.DiskName,
		DiskID:     record.Status.ID,
		ClaimUID:   record.Spec.ClaimRef.UID,
	}
}

// Unclaim returns a disk to the pool after a launch aborted without attaching
// it. A disk that is no longer detached may still be wired to a half-created
// instance, so it is retired instead of reused.
func (p *Provider) Unclaim(ctx context.Context, name string, claim types.UID) error {
	settled := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		record := &v1alpha1.OxideBootDisk{}
		if err := p.reader.Get(ctx, client.ObjectKey{Name: name}, record); err != nil {
			return client.IgnoreNotFound(err)
		}
		// A record that no longer carries our claim has already been handled by
		// the pool controller or handed to another launch.
		if record.Spec.ClaimRef == nil || record.Spec.ClaimRef.UID != claim ||
			record.Spec.Retire || !record.DeletionTimestamp.IsZero() {
			settled = true
			return nil
		}
		disk, err := p.View(ctx, record)
		if err != nil || disk.State.State() != oxide.DiskStateStateDetached {
			return nil
		}
		stored := record.DeepCopy()
		record.Spec.ClaimRef = nil
		record.Spec.ClaimedAt = nil
		if err := p.kubeClient.Patch(ctx, record, client.MergeFromWithOptions(
			stored,
			client.MergeFromWithOptimisticLock{},
		)); err != nil {
			return err
		}
		settled = true
		return nil
	})
	if err == nil && settled {
		return nil
	}
	return errors.Join(err, p.Retire(ctx, name))
}

func (p *Provider) Retire(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		record := &v1alpha1.OxideBootDisk{}
		if err := p.reader.Get(ctx, client.ObjectKey{Name: name}, record); err != nil {
			return client.IgnoreNotFound(err)
		}
		if record.Spec.Retire {
			return nil
		}
		stored := record.DeepCopy()
		record.Spec.Retire = true
		return p.kubeClient.Patch(ctx, record, client.MergeFromWithOptions(
			stored,
			client.MergeFromWithOptimisticLock{},
		))
	})
}

func (p *Provider) Ensure(
	ctx context.Context,
	record *v1alpha1.OxideBootDisk,
) (*oxide.Disk, error) {
	description := ownership.PoolMarker{
		ClusterUID:   p.clusterUID,
		BootDiskUID:  record.UID,
		NodeClassUID: record.Spec.NodeClassRef.UID,
		ConfigHash:   record.Spec.ConfigHash,
	}.Description()
	disk, err := p.disks.Create(ctx, &oxide.DiskCreate{
		Name:        oxide.Name(record.Spec.DiskName),
		Description: description,
		Size:        oxide.ByteCount(record.Spec.SizeBytes),
		DiskBackend: oxide.DiskBackend{Value: &oxide.DiskBackendDistributed{
			DiskSource: oxide.DiskSource{Value: &oxide.DiskSourceImage{
				ImageId: record.Spec.ImageID,
			}},
		}},
	})
	if err != nil {
		if !errors.Is(err, oxide.ErrObjectAlreadyExists) {
			return nil, err
		}
		disk, err = p.View(ctx, record)
		if err != nil {
			return nil, err
		}
		marker, ok := ownership.ParsePool(disk.Description)
		if !ok || marker.BootDiskUID != record.UID || marker.ClusterUID != p.clusterUID {
			return nil, fmt.Errorf("disk %s is not owned by record %s", disk.Name, record.Name)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return p.disks.WaitDetached(waitCtx, disk)
}

func (p *Provider) View(ctx context.Context, record *v1alpha1.OxideBootDisk) (*oxide.Disk, error) {
	return p.disks.View(ctx, diskRef(record))
}

func (p *Provider) Delete(ctx context.Context, record *v1alpha1.OxideBootDisk) error {
	expected := ownership.PoolMarker{
		ClusterUID:   p.clusterUID,
		BootDiskUID:  record.UID,
		NodeClassUID: record.Spec.NodeClassRef.UID,
		ConfigHash:   record.Spec.ConfigHash,
	}
	return p.deleteOwned(ctx, diskRef(record), expected)
}

func (p *Provider) deleteOwned(
	ctx context.Context,
	ref string,
	expected ownership.PoolMarker,
) error {
	return p.disks.DeleteOwned(ctx, ref, func(disk *oxide.Disk) error {
		marker, ok := ownership.ParsePool(disk.Description)
		if !ok {
			return fmt.Errorf("disk %s has no valid boot-pool ownership marker", disk.Name)
		}
		if marker != expected {
			return fmt.Errorf(
				"disk %s does not match its expected boot-pool owner",
				disk.Name,
			)
		}
		return nil
	})
}

func (p *Provider) CollectOrphans(ctx context.Context, known map[types.UID]struct{}) error {
	disks, err := p.disks.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range disks {
		disk := &disks[i]
		marker, ok := ownership.ParsePool(disk.Description)
		if !ok || marker.ClusterUID != p.clusterUID {
			continue
		}
		if _, ok := known[marker.BootDiskUID]; ok {
			continue
		}
		if disk.State.State() != oxide.DiskStateStateDetached {
			continue
		}
		err := p.deleteOwned(ctx, disk.Id, marker)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func diskRef(record *v1alpha1.OxideBootDisk) string {
	if record.Status.ID != "" {
		return record.Status.ID
	}
	return record.Spec.DiskName
}
