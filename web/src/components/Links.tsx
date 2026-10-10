export const REPO = "https://github.com/jordanfelle/consolidarr";

// versionLink points at the exact build: a commit for main-<sha> builds, the release for tags, else the repo.
export function versionLink(v?: string): string {
  if (!v) return REPO;
  const commit = /^main-([0-9a-f]{7,40})$/.exec(v);
  if (commit) return `${REPO}/commit/${commit[1]}`;
  if (/^v\d/.test(v)) return `${REPO}/releases/tag/${v}`;
  return REPO;
}

// Inline mark: lucide dropped brand icons in v1.
export function GitHubMark({ size = 14 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
    </svg>
  );
}

const link = "inline-flex items-center gap-1.5 hover:text-white";

// Footer: version (linked to the exact build), repository, issues, API reference and licence.
export function Footer({ version, className = "" }: { version?: string; className?: string }) {
  return (
    <div className={`flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted ${className}`}>
      <a className={link} href={REPO} target="_blank" rel="noreferrer"><GitHubMark /> GitHub</a>
      {version && <a className={link} href={versionLink(version)} target="_blank" rel="noreferrer" title="The exact build you are running">{version}</a>}
      <a className={link} href={`${REPO}/issues`} target="_blank" rel="noreferrer">Report an issue</a>
      <a className={link} href="/api/docs" target="_blank" rel="noreferrer">API</a>
      <a className={link} href={`${REPO}/blob/main/LICENSE`} target="_blank" rel="noreferrer">AGPL-3.0</a>
    </div>
  );
}
