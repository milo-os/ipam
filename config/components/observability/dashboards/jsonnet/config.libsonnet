// Shared configuration for IPAM service observability.
//
// Referenced by every dashboard under dashboards/jsonnet/ and by alert/
// recording rule mixins. Centralises selectors and constants so queries
// do not hard-code values in dozens of places.
{
  dashboards: {
    refresh: '30s',
    timezone: 'utc',
    timeRange: { from: 'now-1h', to: 'now' },
    // Tags applied to every IPAM Grafana dashboard.
    tags: ['ipam', 'milo', 'platform'],
    // Grafana folder displayed in the Grafana UI.
    folder: 'Platform / IPAM',
  },

  // The IPAM apiserver job is selected dynamically by the dashboard's
  // $job template variable (populated via label_values on apiserver_request_total)
  // so dashboards do not hard-code the label value produced by
  // prometheus-operator's ServiceMonitor conventions.
  //
  // Resource label values from the IPAM metrics spec:
  //   resource: IPPrefixClaim | IPAddressClaim | ASNClaim
  //   ip_family: IPv4 | IPv6 | N/A
  //   outcome:   success | pool_exhausted | conflict | error
}
