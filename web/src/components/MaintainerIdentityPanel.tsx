"use client";

import Link from "next/link";
import { Card } from "clo-ui/components/Card";
import styles from "./MaintainerIdentityPanel.module.css";

export type IdentityObservation = {
  source: string;
  sourceRef?: string;
  name?: string;
  email?: string;
  githubUser?: string;
  lfid?: string;
  companyName?: string;
  matchStatus?: string;
  matchReason?: string;
  confidence?: string;
  projectId?: number;
  observedAt: string;
  sourceUserId?: string;
  sourceUserType?: string;
  sourceGithubId?: string;
  sourceLastModifiedAt?: string;
  identityCount?: number;
  sourceFilePath?: string;
  sourceLine?: number;
  sourceCommitSha?: string;
  sourceLineUrl?: string;
  sourcePrNumber?: number;
  sourcePrUrl?: string;
  sourceReviewState?: string;
};

type MaintainerIdentityPanelProps = {
  observations: IdentityObservation[];
};

const matchStatusLabel: Record<string, string> = {
  matched: "Matched",
  unmatched: "Unmatched",
  candidate: "Candidate",
  conflict: "Conflict",
  chosen: "Chosen",
  duplicate: "Duplicate",
  error: "Error",
};

const confidenceLabel: Record<string, string> = {
  exact: "Exact",
  strong: "Strong",
  high: "High",
  medium: "Medium",
  weak: "Weak",
  low: "Low",
};

const sourceLabel: Record<string, string> = {
  lfx: "LFX",
  "foundation-csv": "Foundation CSV",
  "dot-project": ".project maintainers",
  "legacy-ref": "Legacy ref",
};

const reviewStateLabel: Record<string, string> = {
  approved: "PR approved",
  unreviewed: "PR unreviewed",
  "direct-push": "Direct push",
  unknown: "Review unknown",
};

function formatDateTime(value?: string | null) {
  if (!value) {
    return "—";
  }
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return "—";
  }
  return parsed.toLocaleString("en-US", {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

function MatchBadge({ status, confidence }: { status?: string; confidence?: string }) {
  const label = (status ? matchStatusLabel[status] || status : null) ?? "—";
  const confLabel = confidence ? confidenceLabel[confidence] || confidence : null;
  return (
    <span className={styles.matchCell}>
      <span className={`${styles.badge} ${styles[`badge_${status}`] || styles.badge_default}`}>
        {label}
      </span>
      {confLabel && (
        <span className={`${styles.badge} ${styles.badge_confidence}`}>{confLabel}</span>
      )}
    </span>
  );
}

export default function MaintainerIdentityPanel({
  observations,
}: MaintainerIdentityPanelProps) {
  if (!observations || observations.length === 0) {
    return null;
  }

  return (
    <section className={styles.panel}>
      <Card hoverable={false} className={styles.card}>
        <div className={styles.cardContent}>
          <div className={styles.header}>
            <div>
              <h2 className={styles.title}>Identity observations</h2>
              <p className={styles.subtitle}>
                Raw identity signals recorded from external sources during enrichment.
              </p>
            </div>
            <span className={styles.count}>{observations.length}</span>
          </div>

          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Source</th>
                  <th>Name</th>
                  <th>GitHub</th>
                  <th>Email</th>
                  <th>LFID</th>
                  <th>Company</th>
                  <th>Match</th>
                  <th>Observed</th>
                </tr>
              </thead>
              <tbody>
                {observations.map((obs, i) => (
                  <tr key={i}>
                    <td>
                      <span className={styles.sourceLabel}>
                        {sourceLabel[obs.source] || obs.source}
                      </span>
                      {obs.sourceRef && (
                        <span className={styles.sourceRef}>{obs.sourceRef}</span>
                      )}
                      {obs.sourceLineUrl && obs.sourceFilePath && (
                        <a
                          className={styles.sourceLineLink}
                          href={obs.sourceLineUrl}
                          target="_blank"
                          rel="noreferrer"
                        >
                          {obs.sourceFilePath}
                          {obs.sourceLine ? `#L${obs.sourceLine}` : ""}
                        </a>
                      )}
                      {(obs.sourcePrUrl || obs.sourceReviewState) && (
                        <span className={styles.provenanceCell}>
                          {obs.sourcePrUrl && (
                            <a
                              className={styles.prLink}
                              href={obs.sourcePrUrl}
                              target="_blank"
                              rel="noreferrer"
                            >
                              PR #{obs.sourcePrNumber}
                            </a>
                          )}
                          {obs.sourceReviewState && (
                            <span
                              className={`${styles.badge} ${
                                styles[`badge_review_${obs.sourceReviewState}`] ||
                                styles.badge_default
                              }`}
                            >
                              {reviewStateLabel[obs.sourceReviewState] || obs.sourceReviewState}
                            </span>
                          )}
                        </span>
                      )}
                      {obs.projectId != null && (
                        <Link
                          className={styles.projectLink}
                          href={`/projects/${obs.projectId}`}
                        >
                          project #{obs.projectId}
                        </Link>
                      )}
                    </td>
                    <td>{obs.name || "—"}</td>
                    <td>{obs.githubUser || "—"}</td>
                    <td>{obs.email || "—"}</td>
                    <td>{obs.lfid || "—"}</td>
                    <td>{obs.companyName || "—"}</td>
                    <td>
                      <MatchBadge status={obs.matchStatus} confidence={obs.confidence} />
                      {obs.matchReason && (
                        <span className={styles.matchReason}>{obs.matchReason}</span>
                      )}
                    </td>
                    <td className={styles.dateCell}>{formatDateTime(obs.observedAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </Card>
    </section>
  );
}
