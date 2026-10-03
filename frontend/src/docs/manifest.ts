// Docs table of contents. Each article maps to a Markdown file under
// ./content/<file>.md (loaded via Vite glob in the Docs page). Order here is the
// order shown in the navigation rail.

export interface DocArticle {
  slug: string;   // URL segment
  title: string;
  file: string;   // path under ./content (without .md)
}
export interface DocCategory {
  slug: string;
  title: string;
  icon: string;   // lucide icon name
  articles: DocArticle[];
}

export const categories: DocCategory[] = [
  {
    slug: "getting-started",
    title: "Getting Started",
    icon: "Rocket",
    articles: [
      { slug: "overview", title: "What DockBack is & how it's secured", file: "getting-started/overview" },
      { slug: "first-login", title: "First login & changing your password", file: "getting-started/first-login" },
      { slug: "dashboard", title: "Dashboard tour", file: "getting-started/dashboard" },
      { slug: "sizing", title: "Requirements, sizing & performance", file: "getting-started/sizing" },
    ],
  },
  {
    slug: "nodes",
    title: "Connecting Servers (Nodes)",
    icon: "Server",
    articles: [
      { slug: "transports", title: "Transports compared — which to pick", file: "nodes/transports" },
      { slug: "local-host", title: "The local host (bundled proxy)", file: "nodes/local-host" },
      { slug: "remote-linux", title: "A remote Linux server (no SSH)", file: "nodes/remote-linux" },
      { slug: "synology", title: "A Synology NAS (no SSH)", file: "nodes/synology" },
      { slug: "ssh", title: "Connect over SSH", file: "nodes/ssh" },
      { slug: "mtls", title: "Connect over daemon mTLS", file: "nodes/mtls" },
      { slug: "private-network", title: "Reach nodes over Tailscale / WireGuard", file: "nodes/private-network" },
      { slug: "clusters", title: "Clusters — grouping your servers", file: "nodes/clusters" },
      { slug: "machine", title: "The Machine page — hardware & live usage", file: "nodes/machine" },
      { slug: "manage", title: "Edit, test, remove & troubleshoot", file: "nodes/manage" },
    ],
  },
  {
    slug: "backups",
    title: "Creating Backups",
    icon: "CloudUpload",
    articles: [
      { slug: "concepts", title: "What a backup captures & verification", file: "backups/concepts" },
      { slug: "manual", title: "Run a manual backup (every option)", file: "backups/manual" },
      { slug: "multi", title: "Back up many containers at once", file: "backups/multi" },
      { slug: "databases", title: "Database-aware consistent dumps", file: "backups/databases" },
      { slug: "incremental", title: "Incremental volume backups (changed files only)", file: "backups/incremental" },
      { slug: "tripwire", title: "Ransomware tripwire (mass-change detection)", file: "backups/tripwire" },
      { slug: "app-native", title: "App-native exports (Paperless, Nextcloud)", file: "backups/app-native" },
      { slug: "hooks", title: "Quiesce hooks (pre/post)", file: "backups/hooks" },
      { slug: "pangolin", title: "Pangolin & an application that is several containers", file: "backups/pangolin" },
      { slug: "audiobookshelf", title: "Audiobookshelf & embedded-database apps", file: "backups/audiobookshelf" },
      { slug: "bookstack", title: "BookStack & env-configured apps", file: "backups/bookstack" },
      { slug: "calibre", title: "Calibre & apps that share a library", file: "backups/calibre" },
      { slug: "commafeed", title: "CommaFeed & apps with two storage backends", file: "backups/commafeed" },
      { slug: "credential-stores", title: "Dockhand & apps that hold other systems' keys", file: "backups/credential-stores" },
      { slug: "gotify", title: "Gotify & apps whose data is the credentials", file: "backups/gotify" },
      { slug: "guacamole", title: "Guacamole & all-in-one images with a database inside", file: "backups/guacamole" },
      { slug: "homepage", title: "Homepage & the smallest backup that matters most", file: "backups/homepage" },
      { slug: "immich", title: "Immich & backing up a large photo library", file: "backups/immich" },
      { slug: "jellyfin", title: "Jellyfin & backing up only what you can't rebuild", file: "backups/jellyfin" },
      { slug: "karakeep", title: "Karakeep & backing up AI-generated data", file: "backups/karakeep" },
      { slug: "linux-update-dashboard", title: "Linux Update Dashboard & split key material", file: "backups/linux-update-dashboard" },
      { slug: "mealie", title: "Mealie & three pieces that must match", file: "backups/mealie" },
      { slug: "nextcloud", title: "Nextcloud & the backup that has to be undone", file: "backups/nextcloud" },
      { slug: "nginx-proxy-manager", title: "Nginx Proxy Manager & two volumes that are one thing", file: "backups/nginx-proxy-manager" },
      { slug: "paperless", title: "Paperless-ngx & two backups that do different jobs", file: "backups/paperless" },
      { slug: "plex", title: "Plex & backing up a server that lives in the cloud's address book", file: "backups/plex" },
      { slug: "radicale", title: "Radicale & noticing what nobody looks at", file: "backups/radicale" },
      { slug: "tautulli", title: "Tautulli & the address that breaks when something else moves", file: "backups/tautulli" },
      { slug: "termix", title: "Termix & the archive that is your whole fleet", file: "backups/termix" },
      { slug: "uptime-kuma", title: "Uptime Kuma & one image, two databases", file: "backups/uptime-kuma" },
      { slug: "wikijs", title: "Wiki.js & a backup that is almost entirely one dump", file: "backups/wikijs" },
      { slug: "cancel", title: "Cancel a running backup", file: "backups/cancel" },
    ],
  },
  {
    slug: "scheduling",
    title: "Scheduling & Retention",
    icon: "CalendarClock",
    articles: [
      { slug: "policy", title: "The global backup policy", file: "scheduling/policy" },
      { slug: "per-container", title: "Per-container overrides", file: "scheduling/per-container" },
      { slug: "schedules", title: "Scheduled (automatic) backups", file: "scheduling/schedules" },
      { slug: "labels", title: "Label-driven policy (dockback.* labels)", file: "scheduling/labels" },
      { slug: "notifications", title: "Notifications", file: "scheduling/notifications" },
    ],
  },
  {
    slug: "destinations",
    title: "External Backup Destinations",
    icon: "HardDrive",
    articles: [
      { slug: "overview", title: "Overview, 3-2-1 & least privilege", file: "destinations/overview" },
      { slug: "synology-smb", title: "Synology NAS (SMB)", file: "destinations/synology-smb" },
      { slug: "samba", title: "Samba / SMB share", file: "destinations/samba" },
      { slug: "nextcloud", title: "Nextcloud (WebDAV)", file: "destinations/nextcloud" },
      { slug: "s3", title: "S3 / Backblaze B2", file: "destinations/s3" },
      { slug: "sftp", title: "SSH (SFTP)", file: "destinations/sftp" },
      { slug: "testing", title: "Testing & capacity", file: "destinations/testing" },
    ],
  },
  {
    slug: "restore",
    title: "Restoring",
    icon: "RotateCcw",
    articles: [
      { slug: "concepts", title: "How restore works", file: "restore/concepts" },
      { slug: "version-source", title: "Restore by version & source location", file: "restore/version-source" },
      { slug: "partial", title: "Volumes-only or database-only", file: "restore/partial" },
      { slug: "disaster", title: "Disaster recovery (recreate a container)", file: "restore/disaster" },
      { slug: "stack", title: "One-click stack restore", file: "restore/stack" },
      { slug: "migrate-stack", title: "Moving a stack to another server", file: "restore/migrate-stack" },
      { slug: "site-address", title: "Moving an app to a different address", file: "restore/site-address" },
      { slug: "download", title: "Download a decrypted archive", file: "restore/download" },
      { slug: "without-dockback", title: "Restore without DockBack (offline tool)", file: "restore/without-dockback" },
      { slug: "standby", title: "Pilot-light standby (prove failover to another node)", file: "restore/standby" },
    ],
  },
  {
    slug: "security",
    title: "Security & Operations",
    icon: "ShieldCheck",
    articles: [
      { slug: "encryption-key", title: "Back up your encryption key (critical)", file: "security/encryption-key" },
      { slug: "write-only", title: "Write-only backups (DockBack can't read them)", file: "security/write-only" },
      { slug: "app-backup", title: "Back up & restore DockBack itself", file: "security/app-backup" },
      { slug: "two-factor", title: "Two-factor authentication (2FA)", file: "security/two-factor" },
      { slug: "socket-proxy", title: "Socket-proxy permissions explained", file: "security/socket-proxy" },
      { slug: "tls", title: "HTTPS & TLS", file: "security/tls" },
      { slug: "path-confinement", title: "Destination path confinement", file: "security/path-confinement" },
      { slug: "egress", title: "Egress allow-list (outbound control)", file: "security/egress" },
      { slug: "api-tokens", title: "API tokens for automation", file: "security/api-tokens" },
      { slug: "audit", title: "Audit trail", file: "security/audit" },
      { slug: "logs", title: "Logs & live streaming", file: "security/logs" },
    ],
  },
];
