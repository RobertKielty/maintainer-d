"use client";

import { Card } from "clo-ui/components/Card";
import { IdentityObservation } from "./MaintainerIdentityPanel";
import styles from "./LfxProfilesPanel.module.css";

type LfxProfilesPanelProps = {
  observations: IdentityObservation[];
  // The profile ID stored on the maintainer record itself. Authoritative:
  // auto-add links a profile at creation time, but the enrichment pass skips
  // maintainers that already have any lfx observation row, so the
  // observations below can still say "unmatched" long after a profile was
  // linked. The panel must render the linked profile from the record even
  // when no observation row corroborates it.
  lfxUserId?: string | null;
};

const statusLabel: Record<string, string> = {
  matched: "Matched",
  chosen: "Chosen",
  duplicate: "Duplicate",
  error: "Error",
  unmatched: "Unmatched",
  linked: "Linked",
};

const typeLabel: Record<string, string> = {
  contact: "Contact",
  lead: "Lead",
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

function StatusBadge({ status }: { status?: string }) {
  const label = (status ? statusLabel[status] || status : null) ?? "—";
  return (
    <span className={`${styles.badge} ${styles[`badge_${status}`] || styles.badge_default}`}>
      {label}
    </span>
  );
}

function TypeBadge({ userType }: { userType?: string }) {
  if (!userType) {
    return <span className={styles.muted}>—</span>;
  }
  return (
    <span className={`${styles.badge} ${styles[`badge_type_${userType}`] || styles.badge_default}`}>
      {typeLabel[userType] || userType}
    </span>
  );
}

export default function LfxProfilesPanel({ observations, lfxUserId }: LfxProfilesPanelProps) {
  const linkedProfileId = (lfxUserId || "").trim();
  // Requiring sourceUserId keeps search-level failure rows (an "error"
  // observation with no profile behind it) from rendering as blank profiles.
  const lfxObservations = (observations || []).filter(
    (observation) =>
      observation.source === "lfx" &&
      observation.matchStatus !== "unmatched" &&
      Boolean(observation.sourceUserId)
  );
  // Observations are project-scoped, so a maintainer active on several
  // projects repeats the same LFX profile once per project. Deduplicate by
  // profile (keeping the newest observation) so the count - and the
  // duplicate-profile warning it drives - reflects distinct LFX profiles.
  const byProfile = new Map<string, IdentityObservation>();
  for (const observation of lfxObservations) {
    const key = observation.sourceUserId as string;
    const current = byProfile.get(key);
    if (
      !current ||
      new Date(observation.observedAt).getTime() > new Date(current.observedAt).getTime()
    ) {
      byProfile.set(key, observation);
    }
  }
  const profiles = Array.from(byProfile.values());
  // A linked profile on the maintainer record must render even when no
  // observation row corroborates it - hiding the panel here is exactly the
  // stale-observation bug this prop exists to fix.
  if (profiles.length === 0 && !linkedProfileId) {
    return null;
  }
  const linkedProfileObserved = profiles.some(
    (profile) => (profile.sourceUserId || "").trim() === linkedProfileId
  );
  // The multi-profile path can persist only "error" rows when every identity
  // lookup fails nonfatally, so the duplicate-profile notice must not promise
  // a "Chosen" row that does not exist.
  const hasChosen = profiles.some((profile) => profile.matchStatus === "chosen");

  return (
    <section className={styles.panel}>
      <Card hoverable={false} className={styles.card}>
        <div className={styles.cardContent}>
          <div className={styles.header}>
            <div>
              <h2 className={styles.title}>LFX Profiles</h2>
              <p className={styles.subtitle}>
                LFX profile records returned for this maintainer&apos;s GitHub handle or email.
              </p>
            </div>
            <span className={styles.count}>{profiles.length}</span>
          </div>

          {linkedProfileId && (
            <p className={styles.notice}>
              This maintainer record is linked to LFX profile ID{" "}
              <span className={styles.sourceRef}>{linkedProfileId}</span>.
              {!linkedProfileObserved &&
                " No enrichment observation below corroborates this link yet - the linked profile was resolved when the maintainer was added, and the observations shown here have not been refreshed since."}
            </p>
          )}

          {profiles.length > 1 && (
            <p className={styles.notice}>
              This maintainer has more than one LFX profile bound to the same GitHub account.
              This is a known upstream LFX data-quality issue, not a maintainer-d bug — LFX has
              no 1:1 mapping between profile IDs and GitHub identities.{" "}
              {hasChosen
                ? "The row marked “Chosen” is the one maintainer-d treats as canonical; the rest are shown here to help troubleshoot which profile is authoritative."
                : "No row is currently marked “Chosen”: none of these profiles has been selected as canonical yet."}
            </p>
          )}

          {profiles.length > 0 && (
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>LFID</th>
                  <th>Type</th>
                  <th>Company</th>
                  <th>Email</th>
                  <th>Identities</th>
                  <th>Last modified</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {profiles.map((obs, i) => (
                  <tr key={`${obs.sourceUserId || "profile"}-${i}`}>
                    <td>
                      {obs.lfid ? (
                        <a
                          className={styles.lfidLink}
                          href={`https://openprofile.dev/profile/${encodeURIComponent(obs.lfid)}`}
                          target="_blank"
                          rel="noreferrer"
                        >
                          {obs.lfid}
                        </a>
                      ) : (
                        "—"
                      )}
                      {obs.sourceUserId && (
                        <span className={styles.sourceRef}>{obs.sourceUserId}</span>
                      )}
                    </td>
                    <td>
                      <TypeBadge userType={obs.sourceUserType} />
                    </td>
                    <td>{obs.companyName || "—"}</td>
                    <td>{obs.email || "—"}</td>
                    <td>{obs.matchStatus === "error" ? "—" : obs.identityCount ?? "—"}</td>
                    <td className={styles.dateCell}>
                      {formatDateTime(obs.sourceLastModifiedAt)}
                    </td>
                    <td>
                      {(obs.sourceUserId || "").trim() === linkedProfileId &&
                        linkedProfileId && <StatusBadge status="linked" />}
                      <StatusBadge status={obs.matchStatus} />
                      {obs.matchReason && (
                        <span className={styles.matchReason}>{obs.matchReason}</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          )}
        </div>
      </Card>
    </section>
  );
}
