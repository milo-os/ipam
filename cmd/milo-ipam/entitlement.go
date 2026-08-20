package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.datum.net/datumctl/plugin"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ipamServiceName is the service-catalog Service metadata.name for IPAM. It must
// match config/components/service-catalog/service.yaml — the ServiceEntitlement
// references the Service by this name, not by the API group "ipam.miloapis.com".
const ipamServiceName = "ipam-miloapis-com"

// entitlementWatchTimeout bounds how long the interactive request path waits for
// the reconciler to publish the entitlement's Ready condition after Create.
const entitlementWatchTimeout = 15 * time.Second

// EnsureIPAMEntitlement is the preflight the plugin runs before any IPAM API
// call: it verifies the active project has an Active ServiceEntitlement for the
// IPAM service, and offers to request one when it doesn't.
//
// IPAM's Service is self-service, so an entitlement this plugin creates becomes
// Active without anyone approving it. The PendingApproval and Rejected branches
// remain for entitlements created while the service was provider-gated.
//
//   - project == "" (platform scope): no-op. Platform/operator calls are not
//     project-entitled.
//   - Active: proceed.
//   - PendingApproval: stop with a pointer to `datumctl services list`.
//   - Rejected: stop with a pointer to `datumctl services enable`.
//   - none found, or the API is not served in this control plane
//     (IsNoMatchError): prompt-and-request on a TTY, else an actionable error.
//
// out should be cmd.ErrOrStderr() and in should be cmd.InOrStdin() so prompts
// never pollute the structured (-o json|yaml) data contract on stdout.
func EnsureIPAMEntitlement(ctx context.Context, env datumEnv, in io.Reader, out io.Writer) error {
	project := env.project
	if project == "" {
		return nil
	}

	wc, err := newEntitlementClient(env)
	if err != nil {
		return err
	}

	var list servicesv1alpha1.ServiceEntitlementList
	if err := wc.List(ctx, &list); err != nil {
		if apimeta.IsNoMatchError(err) {
			// The service-catalog API isn't served in this project's control
			// plane — treat it the same as "no entitlement exists yet".
			return promptAndRequestEntitlement(ctx, project, wc, in, out)
		}
		return newCLIError(exitUnavailable,
			fmt.Sprintf("could not check IPAM service entitlement for project %q: %v", project, err)).
			withFix("verify you are logged in (datumctl login) and the project is reachable.").
			withCause(err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		if item.Spec.ServiceRef.Name != ipamServiceName {
			continue
		}
		switch item.Status.Phase {
		case servicesv1alpha1.EntitlementPhaseActive:
			return nil
		case servicesv1alpha1.EntitlementPhasePendingApproval:
			return newCLIError(exitForbidden,
				fmt.Sprintf("IPAM is not yet enabled for project %q: the entitlement is still being activated.", project)).
				withFix("check the status with:\n       datumctl services list")
		case servicesv1alpha1.EntitlementPhaseRejected:
			return newCLIError(exitForbidden,
				fmt.Sprintf("the IPAM entitlement request for project %q was rejected.", project)).
				withFix(fmt.Sprintf("submit a new request with:\n       datumctl services enable %s", ipamServiceName))
		}
	}

	return promptAndRequestEntitlement(ctx, project, wc, in, out)
}

// promptAndRequestEntitlement handles the "no entitlement yet" case. On a TTY it
// asks whether to enable IPAM and, if confirmed, creates the ServiceEntitlement
// and watches briefly for it to reach a terminal phase. IPAM is self-service, so
// the happy path is Active. Non-interactively it returns an actionable error
// rather than blocking on an unanswerable prompt.
func promptAndRequestEntitlement(ctx context.Context, project string, wc client.WithWatch, in io.Reader, out io.Writer) error {
	if !isTTYReader(in) {
		return newCLIError(exitForbidden,
			fmt.Sprintf("IPAM is not enabled for project %q.", project)).
			withFix(fmt.Sprintf("enable it with:\n       datumctl services enable %s", ipamServiceName))
	}

	_, _ = fmt.Fprintf(out, "IPAM is not enabled for project %q.\n", project)
	_, _ = fmt.Fprint(out, "Enabling it takes effect right away; no approval is needed.\n")
	_, _ = fmt.Fprint(out, "Would you like to enable it now? [y/N]: ")

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return entitlementNotEnabledErr(project)
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer != "y" && answer != "yes" {
		return entitlementNotEnabledErr(project)
	}

	_, _ = fmt.Fprintf(out, "Enabling IPAM for project %q...\n", project)

	entitlement := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: ipamServiceName},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: ipamServiceName},
		},
	}
	if err := wc.Create(ctx, entitlement); err != nil {
		return newCLIError(exitUnavailable,
			fmt.Sprintf("could not enable IPAM for project %q: %v", project, err)).
			withCause(err)
	}

	// Watch for the reconciler to publish the Ready condition. The expected
	// terminal phase is Active.
	watchCtx, cancel := context.WithTimeout(ctx, entitlementWatchTimeout)
	defer cancel()

	watcher, err := wc.Watch(watchCtx, &servicesv1alpha1.ServiceEntitlementList{})
	if err != nil {
		// The request was created; we just can't observe it. Tell the user how
		// to check, and don't proceed (IPAM still isn't active).
		return pendingApprovalErr(project)
	}
	defer watcher.Stop()

	for {
		select {
		case <-watchCtx.Done():
			_, _ = fmt.Fprintf(out, "\nIPAM for project %q has been requested but is not active yet.\n", project)
			_, _ = fmt.Fprintf(out, "Run your command again once it becomes active.\n\n")
			_, _ = fmt.Fprintf(out, "Check status with: datumctl services list\n")
			return pendingApprovalErr(project)

		case event, ok := <-watcher.ResultChan():
			if !ok {
				return pendingApprovalErr(project)
			}
			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}
			item, ok := event.Object.(*servicesv1alpha1.ServiceEntitlement)
			if !ok || item.Spec.ServiceRef.Name != ipamServiceName {
				continue
			}
			if apimeta.FindStatusCondition(item.Status.Conditions, "Ready") == nil {
				continue
			}
			switch item.Status.Phase {
			case servicesv1alpha1.EntitlementPhaseActive:
				_, _ = fmt.Fprintf(out, "IPAM enabled for project %q.\n\n", project)
				return nil
			case servicesv1alpha1.EntitlementPhasePendingApproval:
				_, _ = fmt.Fprintf(out, "\nIPAM for project %q has been requested but is not active yet.\n", project)
				_, _ = fmt.Fprintf(out, "Check status with: datumctl services list\n")
				return pendingApprovalErr(project)
			case servicesv1alpha1.EntitlementPhaseRejected:
				return newCLIError(exitForbidden,
					fmt.Sprintf("the IPAM entitlement request for project %q was rejected.", project)).
					withFix(fmt.Sprintf("submit a new request with:\n       datumctl services enable %s", ipamServiceName))
			default:
				// Unknown/empty phase with Ready set — surface it rather than loop.
				return pendingApprovalErr(project)
			}
		}
	}
}

// entitlementNotEnabledErr is the "declined / cannot prompt" result.
func entitlementNotEnabledErr(project string) *cliError {
	return newCLIError(exitForbidden,
		fmt.Sprintf("IPAM is not enabled for project %q.", project)).
		withFix(fmt.Sprintf("enable it with:\n       datumctl services enable %s", ipamServiceName))
}

// pendingApprovalErr is the "requested, not active yet" result.
func pendingApprovalErr(project string) *cliError {
	return newCLIError(exitForbidden,
		fmt.Sprintf("IPAM for project %q is not active yet.", project)).
		withFix("check the status with:\n       datumctl services list")
}

// newEntitlementClient builds a controller-runtime client (with Watch) against
// the active project's control plane, reusing the plugin's transport contract:
// the same control-plane URL construction as the IPAM clientset and a fresh
// token from the credentials helper. Only the service-catalog scheme is
// registered.
func newEntitlementClient(env datumEnv) (client.WithWatch, error) {
	if !env.usable() {
		return nil, newCLIError(exitUnavailable,
			"cannot check IPAM service entitlement: DATUM_API_HOST/DATUM_CREDENTIALS_HELPER are not set").
			withFix("run via `datumctl ipam ...`.")
	}

	token, err := plugin.Token()
	if err != nil {
		return nil, newCLIError(exitUnavailable,
			fmt.Sprintf("failed to obtain an access token: %v", err)).
			withFix("re-run `datumctl login` and try again.").withCause(err)
	}

	scheme := runtime.NewScheme()
	if err := servicesv1alpha1.AddToScheme(scheme); err != nil {
		return nil, newCLIError(exitError,
			fmt.Sprintf("registering service-catalog scheme: %v", err)).withCause(err)
	}

	cfg := &rest.Config{
		Host:        controlPlaneHost(env),
		BearerToken: token,
		UserAgent:   userAgent(),
	}

	wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, newCLIError(exitUnavailable,
			fmt.Sprintf("building entitlement client: %v", err)).withCause(err)
	}
	return wc, nil
}

// isTTYReader reports whether in is an interactive terminal, so the preflight can
// choose between prompting and emitting an actionable non-interactive error.
func isTTYReader(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	return termIsTerminal(int(f.Fd()))
}
