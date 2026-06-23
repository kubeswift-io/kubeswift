package gateway

import (
	"context"
	"sort"
	"sync"
	"time"

	connect "connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
	"github.com/projectbeskar/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// MigrationService is the read plane for SwiftMigrations. It mirrors
// GuestService's fan-out/merge; the UI polls it while a migration is in flight
// (a server-stream Watch is a later add).
type MigrationService struct {
	kubeswiftv1connect.UnimplementedMigrationServiceHandler
	pool clientProvider
	auth Authenticator
}

func NewMigrationService(pool clientProvider, auth Authenticator) *MigrationService {
	return &MigrationService{pool: pool, auth: auth}
}

func (s *MigrationService) ListMigrations(ctx context.Context, req *connect.Request[kubeswiftv1.ListMigrationsRequest]) (*connect.Response[kubeswiftv1.ListMigrationsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	clusters := s.targetClusters(req.Msg.GetClusters())
	resp := &kubeswiftv1.ListMigrationsResponse{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cl := range clusters {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			migs, lerr := s.listOne(ctx, cl, id, req.Msg.GetNamespace())
			mu.Lock()
			defer mu.Unlock()
			if lerr != nil {
				resp.Errors = append(resp.Errors, &kubeswiftv1.ClusterError{Cluster: cl, Message: lerr.Error()})
				return
			}
			resp.Migrations = append(resp.Migrations, migs...)
		}(cl)
	}
	wg.Wait()
	// Newest first (an in-flight migration floats to the top), then by name.
	sort.Slice(resp.Migrations, func(i, j int) bool {
		a, b := resp.Migrations[i], resp.Migrations[j]
		if at, bt := a.GetCreatedAt().GetSeconds(), b.GetCreatedAt().GetSeconds(); at != bt {
			return at > bt
		}
		return a.GetRef().GetName() < b.GetRef().GetName()
	})
	return connect.NewResponse(resp), nil
}

func (s *MigrationService) listOne(ctx context.Context, cluster string, id Identity, namespace string) ([]*kubeswiftv1.Migration, error) {
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, err
	}
	res := dyn.Resource(swiftMigrationGVR)
	var ul *unstructured.UnstructuredList
	if namespace != "" {
		ul, err = res.Namespace(namespace).List(ctx, metav1.ListOptions{})
	} else {
		ul, err = res.List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, err
	}
	out := make([]*kubeswiftv1.Migration, 0, len(ul.Items))
	for i := range ul.Items {
		out = append(out, migrationToProto(cluster, &ul.Items[i]))
	}
	return out, nil
}

// targetClusters resolves the selector against the registered members (mirrors
// GuestService.targetClusters).
func (s *MigrationService) targetClusters(sel *kubeswiftv1.ClusterSelector) []string {
	all := s.pool.Members()
	sort.Strings(all)
	if sel == nil || sel.GetAllClusters() || len(sel.GetClusters()) == 0 {
		return all
	}
	registered := make(map[string]bool, len(all))
	for _, m := range all {
		registered[m] = true
	}
	var out []string
	for _, c := range sel.GetClusters() {
		if registered[c] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// migrationToProto denormalizes a SwiftMigration (unstructured) to the UI view.
func migrationToProto(cluster string, u *unstructured.Unstructured) *kubeswiftv1.Migration {
	get := func(fields ...string) string {
		v, _, _ := unstructured.NestedString(u.Object, fields...)
		return v
	}
	m := &kubeswiftv1.Migration{
		Ref:            &kubeswiftv1.ObjectRef{Cluster: cluster, Namespace: u.GetNamespace(), Name: u.GetName()},
		Guest:          get("spec", "guestRef", "name"),
		SourceNode:     get("status", "sourceNode"),
		TargetNode:     firstNonEmptyStr(get("spec", "target", "nodeName"), get("status", "destinationNode")),
		Mode:           firstNonEmptyStr(get("status", "mode"), get("spec", "mode")),
		Phase:          get("status", "phase"),
		PhaseDetail:    get("status", "phaseDetail"),
		FailureReason:  get("status", "failureReason"),
		FailureMessage: get("status", "failureMessage"),
	}
	if p, ok, _ := unstructured.NestedInt64(u.Object, "status", "transferProgress"); ok {
		m.TransferProgress = int32(p)
	}
	m.ObservedDowntimeSeconds = durationSeconds(get("status", "observedDowntime"))
	m.ObservedTransferSeconds = durationSeconds(get("status", "observedTransferDuration"))
	if ts := u.GetCreationTimestamp(); !ts.IsZero() {
		m.CreatedAt = timestamppb.New(ts.Time)
	}
	if t := get("status", "terminalAt"); t != "" {
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			m.TerminalAt = timestamppb.New(parsed)
		}
	}
	return m
}

// durationSeconds parses a metav1.Duration string ("1.8s", "2m3s") to seconds.
func durationSeconds(s string) float64 {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d.Seconds()
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
