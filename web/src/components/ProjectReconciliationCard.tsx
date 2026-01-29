"use client";

import { useEffect, useMemo, useState } from "react";
import { Card } from "clo-ui/components/Card";
import ReactMarkdown from "react-markdown";
import { Prism as SyntaxHighlighter } from "react-syntax-highlighter";
import { atomDark } from "react-syntax-highlighter/dist/esm/styles/prism";
import rehypeRaw from "rehype-raw";
import rehypeSanitize, { defaultSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";
import Link from "next/link";
import styles from "./ProjectReconciliationCard.module.css";
import ProjectDiffControl from "./ProjectDiffControl";
import ProjectAddMaintainerModal from "./ProjectAddMaintainerModal";

type MaintainerSummary = {
  id: number;
  name: string;
  github: string;
  inMaintainerRef: boolean;
  status?: string;
  company?: string;
};

type ServiceSummary = {
  id: number;
  name: string;
  description: string;
};

type ProjectSuggestion = {
  id: number;
  name: string;
};

export type AddMaintainerPayload = {
  name: string;
  githubHandle: string;
  email: string;
  company: string;
  companyMode: "select" | "new";
  refLine: string;
};

type ProjectReconciliationCardProps = {
  name: string;
  maturity: string;
  maintainerRef?: string | null;
  maintainerRefStatus: {
    url?: string;
    status: string;
    checkedAt?: string | null;
  };
  maintainerRefBody?: string | null;
  refLines?: Record<string, string>;
  refOnlyGitHub: string[];
  companyOptions?: string[];
  parentProjectId?: number | null;
  onboardingIssue?: string | null;
  mailingList?: string | null;
  maintainers: MaintainerSummary[];
  services: ServiceSummary[];
  createdAt?: string | null;
  updatedAt?: string | null;
  updatedBy?: string | null;
  updatedAuditId?: number | null;
  apiBaseUrl?: string;
  onUpdateMaturity?: (next: string) => Promise<void>;
  onRefresh?: () => void;
  isRefreshing?: boolean;
  canEdit?: boolean;
  onAddMaintainer?: (payload: AddMaintainerPayload) => Promise<void>;
  onUpdateMaintainerRef?: (ref: string) => Promise<void>;
  onUpdateParentProject?: (parentProjectId: number | null) => Promise<void>;
  onBulkStatusChange?: (ids: number[], status: string) => Promise<void>;
};

const formatDate = (value?: string | null) => {
  if (!value) {
    return "—";
  }
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return "—";
  }
  const weekdays = ["SUN", "MON", "TUE", "WED", "THUR", "FRI", "SAT"];
  const months = [
    "JAN",
    "FEB",
    "MAR",
    "APR",
    "MAY",
    "JUN",
    "JUL",
    "AUG",
    "SEP",
    "OCT",
    "NOV",
    "DEC",
  ];
  const weekday = weekdays[parsed.getDay()];
  const month = months[parsed.getMonth()];
  const day = String(parsed.getDate()).padStart(2, "0");
  const year = parsed.getFullYear();
  return `${weekday} ${month} ${day} ${year}`;
};

const maintainerRefSchema = {
  ...defaultSchema,
  tagNames: [
    ...(defaultSchema.tagNames || []),
    "table",
    "thead",
    "tbody",
    "tfoot",
    "tr",
    "th",
    "td",
    "img",
  ],
  attributes: {
    ...(defaultSchema.attributes || {}),
    a: [...(defaultSchema.attributes?.a || []), "target", "rel"],
    img: ["src", "alt", "title", "width", "height"],
    table: ["align"],
    th: ["align", "colspan", "rowspan"],
    td: ["align", "colspan", "rowspan"],
  },
};

const isYamlRef = (value: string) => /\.(ya?ml)(\?|#|$)/i.test(value);

export default function ProjectReconciliationCard({
  name,
  maturity,
  maintainerRef,
  maintainerRefStatus,
  maintainerRefBody,
  refLines,
  refOnlyGitHub,
  companyOptions = [],
  parentProjectId,
  maintainers,
  createdAt,
  updatedAt,
  updatedBy,
  updatedAuditId,
  apiBaseUrl,
  onUpdateMaturity,
  onRefresh,
  isRefreshing,
  canEdit = false,
  onAddMaintainer,
  onUpdateMaintainerRef,
  onUpdateParentProject,
  onBulkStatusChange,
}: ProjectReconciliationCardProps) {
  const refStatus = maintainerRefStatus?.status || "missing";
  const refCheckedAt = maintainerRefStatus?.checkedAt || null;
  const refUrl = maintainerRefStatus?.url || maintainerRef || "";
  const refBody = maintainerRefBody?.trim() ?? "";
  const refMatchCount = maintainers.filter((maintainer) => maintainer.inMaintainerRef).length;
  const refMissingCount = maintainers.length - refMatchCount;
  const refOnlyCount = refOnlyGitHub.length;
  const normalizedRefLines = useMemo(() => {
    const entries = Object.entries(refLines ?? {}).map(([key, value]) => [key.toLowerCase(), value]);
    return Object.fromEntries(entries) as Record<string, string>;
  }, [refLines]);

  const [selectedMaintainers, setSelectedMaintainers] = useState<Set<number>>(new Set());
  const toggleSelected = (id: number) => {
    setSelectedMaintainers((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const clearSelection = () => setSelectedMaintainers(new Set());

  const groupedMaintainers = useMemo(() => {
    const order = ["active", "archived", "emeritus", "retired"];
    const labels: Record<string, string> = {
      active: "Active",
      archived: "Archived",
      emeritus: "Emeritus",
      retired: "Retired",
    };
    const groups: { key: string; label: string; items: MaintainerSummary[] }[] = order.map((k) => ({
      key: k,
      label: labels[k],
      items: [],
    }));
    const bucket: Record<string, MaintainerSummary[]> = {};
    for (const m of maintainers) {
      const key = (m.status || "").toLowerCase();
      bucket[key] = bucket[key] || [];
      bucket[key].push(m);
    }
    return groups
      .map((g) => ({
        ...g,
        items: bucket[g.key] || [],
      }))
      .filter((g) => g.items.length > 0);
  }, [maintainers]);

  const renderMaintainerGroups = () => (
    <div className={styles.groupStack}>
      {groupedMaintainers.map((group) => (
        <div key={group.key} className={styles.group}>
          <div className={styles.groupHeader}>{group.label}</div>
          {group.items.length > 1 ? (
            <label className={styles.selectAll}>
              <input
                type="checkbox"
                checked={group.items.every((m) => selectedMaintainers.has(m.id))}
                onChange={(e) => {
                  const allSelected = e.target.checked;
                  setSelectedMaintainers((prev) => {
                    const next = new Set(prev);
                    if (allSelected) {
                      group.items.forEach((m) => next.add(m.id));
                    } else {
                      group.items.forEach((m) => next.delete(m.id));
                    }
                    return next;
                  });
                }}
              />
              Select all
            </label>
          ) : null}
          <ul className={styles.list}>
            {group.items.map((maintainer) => {
              const checked = selectedMaintainers.has(maintainer.id);

              return (
                <li key={maintainer.id} className={styles.listItem}>
                  <div className={styles.listRow}>
                    <input
                      type="checkbox"
                      className={styles.checkbox}
                      checked={checked}
                      onChange={() => toggleSelected(maintainer.id)}
                    />
                    <Link className={styles.link} href={`/maintainers/${maintainer.id}`}>
                      {maintainer.name || maintainer.github || "Unknown maintainer"}
                    </Link>
                    {maintainer.github ? <span className={styles.secondary}>@{maintainer.github}</span> : null}
                    {maintainer.company ? <span className={styles.secondary}>{maintainer.company}</span> : null}
                    {refStatus === "fetched" ? (
                      maintainer.inMaintainerRef ? null : (
                        <span className={`${styles.statusBadge} ${styles.statusWarn}`}>NOT PRESENT</span>
                      )
                    ) : (
                      <span className={`${styles.statusBadge} ${styles.statusMuted}`}>Not checked</span>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
      <div className={styles.bulkActions}>
        <span className={styles.secondary}>
          {selectedMaintainers.size > 0 ? `${selectedMaintainers.size} selected` : "No maintainers selected"}
        </span>
        <div className={styles.bulkButtons}>
          <button
            type="button"
            className={styles.bulkButton}
            disabled={selectedMaintainers.size === 0}
            onClick={() => updateMaintainerStatuses("Active")}
          >
            Activate
          </button>
          <button
            type="button"
            className={styles.bulkButton}
            disabled={selectedMaintainers.size === 0}
            onClick={() => updateMaintainerStatuses("Emeritus")}
          >
            Emeritus
          </button>
          <button
            type="button"
            className={styles.bulkButton}
            disabled={selectedMaintainers.size === 0}
            onClick={() => updateMaintainerStatuses("Retired")}
          >
            Retire
          </button>
          <button
            type="button"
            className={`${styles.bulkButton} ${styles.bulkDanger}`}
            disabled={selectedMaintainers.size === 0}
            onClick={() => updateMaintainerStatuses("Archived")}
          >
            Archive
          </button>
          <button type="button" className={styles.bulkClear} onClick={clearSelection}>
            Clear
          </button>
        </div>
      </div>
    </div>
  );

  const updateMaintainerStatuses = async (status: string) => {
    if (!onBulkStatusChange) return;
    const ids = Array.from(selectedMaintainers);
    if (ids.length === 0) return;
    await onBulkStatusChange(ids, status);
    clearSelection();
  };
  const isRefBroken = Boolean(refUrl) && refStatus !== "fetched";
  const [modalOpen, setModalOpen] = useState(false);
  const [draft, setDraft] = useState<AddMaintainerPayload | null>(null);
  const [refInput, setRefInput] = useState("");
  const [refSaving, setRefSaving] = useState(false);
  const [refError, setRefError] = useState<string | null>(null);
  const [activeSection, setActiveSection] = useState<string>("legacy");
  const [refEditing, setRefEditing] = useState(false);
  const [parentQuery, setParentQuery] = useState("");
  const [parentSuggestions, setParentSuggestions] = useState<ProjectSuggestion[]>([]);
  const [parentHighlightIndex, setParentHighlightIndex] = useState(-1);
  const [parentSaving, setParentSaving] = useState(false);
  const [parentError, setParentError] = useState<string | null>(null);
  const [parentSelection, setParentSelection] = useState<ProjectSuggestion | null>(null);
  const [currentParentName, setCurrentParentName] = useState<string | null>(null);
  const [maturityModalOpen, setMaturityModalOpen] = useState(false);
  const [maturitySaving, setMaturitySaving] = useState(false);
  const [maturityError, setMaturityError] = useState<string | null>(null);
  useEffect(() => {
    if ((isRefBroken || refEditing) && refInput.trim() === "" && refUrl) {
      setRefInput(refUrl);
    }
  }, [isRefBroken, refEditing, refInput, refUrl]);

  useEffect(() => {
    if (!apiBaseUrl || !parentProjectId) {
      setCurrentParentName(null);
      return;
    }
    let alive = true;
    const loadParent = async () => {
      try {
        const response = await fetch(`${apiBaseUrl}/projects/${parentProjectId}`, {
          credentials: "include",
        });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as { name?: string };
        if (alive) {
          setCurrentParentName((data.name || "").trim() || null);
        }
      } catch {
        // Ignore.
      }
    };
    void loadParent();
    return () => {
      alive = false;
    };
  }, [apiBaseUrl, parentProjectId]);

  useEffect(() => {
    const query = parentQuery.trim();
    if (!apiBaseUrl || query === "") {
      setParentSuggestions([]);
      setParentHighlightIndex(-1);
      return;
    }
    const controller = new AbortController();
    const handle = window.setTimeout(async () => {
      try {
        const response = await fetch(
          `${apiBaseUrl}/projects?namePrefix=${encodeURIComponent(query)}&limit=12&offset=0`,
          { credentials: "include", signal: controller.signal }
        );
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as { projects?: ProjectSuggestion[] };
        const queryLower = query.toLowerCase();
        const nextSuggestions = (data.projects || []).filter((project) =>
          project.name.toLowerCase().startsWith(queryLower)
        );
        setParentSuggestions(nextSuggestions);
        setParentHighlightIndex(nextSuggestions.length > 0 ? 0 : -1);
      } catch (err) {
        if ((err as Error).name === "AbortError") {
          return;
        }
        setParentSuggestions([]);
        setParentHighlightIndex(-1);
      }
    }, 300);
    return () => {
      controller.abort();
      window.clearTimeout(handle);
    };
  }, [apiBaseUrl, parentQuery]);

  const handleParentSelect = (project: ProjectSuggestion) => {
    setParentSelection(project);
    setParentQuery(project.name);
    setParentSuggestions([]);
    setParentHighlightIndex(-1);
    setParentError(null);
  };

  const handleParentKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (parentSuggestions.length === 0) {
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setParentHighlightIndex((current) => {
        const next = current + 1;
        return next >= parentSuggestions.length ? 0 : next;
      });
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setParentHighlightIndex((current) => {
        const next = current - 1;
        return next < 0 ? parentSuggestions.length - 1 : next;
      });
    } else if (event.key === "Enter") {
      event.preventDefault();
      const selected =
        parentHighlightIndex >= 0 ? parentSuggestions[parentHighlightIndex] : null;
      if (selected) {
        handleParentSelect(selected);
      }
    }
  };

  const dotProjectSection = (
    <div className={styles.section}>
      <h3 className={styles.subSectionTitle}>Proposed dot project.yaml</h3>
      <p className={styles.stub}>
        Coming soon: this section will combine CNCF database fields and the Project Admin File to propose a standardized{" "}
        <code>project.yaml</code> that projects can check in for GitOps-friendly maintainer rosters, mailing lists, and
        service metadata.
      </p>
      <div className={styles.parentProject}>
        <h3 className={styles.subSectionTitle}>Parent Project</h3>
        <div className={styles.parentCurrent}>
          {parentProjectId ? (
            <Link className={styles.parentLink} href={`/projects/${parentProjectId}`}>
              {currentParentName || `Project #${parentProjectId}`}
            </Link>
          ) : (
            <span className={styles.parentEmpty}>No parent project set.</span>
          )}
        </div>
        {canEdit && onUpdateParentProject ? (
          <div className={styles.parentField}>
            <input
              className={styles.parentInput}
              value={parentQuery}
              onChange={(event) => {
                setParentQuery(event.target.value);
                setParentSelection(null);
                setParentError(null);
              }}
              onKeyDown={handleParentKeyDown}
              placeholder="Search for a parent project"
            />
            {parentQuery.trim() !== "" ? (
              <div className={styles.parentPopup} role="listbox">
                <div className={styles.parentList}>
                  {parentSuggestions.length > 0 ? (
                    parentSuggestions.map((project, index) => (
                      <button
                        key={project.id}
                        type="button"
                        className={`${styles.parentOption} ${
                          index === parentHighlightIndex ? styles.parentOptionActive : ""
                        }`}
                        onClick={() => handleParentSelect(project)}
                        role="option"
                        aria-selected={index === parentHighlightIndex}
                      >
                        {project.name}
                      </button>
                    ))
                  ) : (
                    <div className={styles.parentEmpty}>No matching projects.</div>
                  )}
                </div>
              </div>
            ) : null}
            <div className={styles.parentActions}>
              <button
                className={styles.parentSave}
                type="button"
                disabled={parentSaving || !parentSelection}
                onClick={async () => {
                  if (!onUpdateParentProject || !parentSelection) {
                    setParentError("Select a parent project.");
                    return;
                  }
                  setParentSaving(true);
                  setParentError(null);
                  try {
                    await onUpdateParentProject(parentSelection.id);
                    setParentQuery("");
                    setParentSelection(null);
                    setCurrentParentName(parentSelection.name);
                    if (onRefresh) {
                      onRefresh();
                    }
                  } catch {
                    setParentError("Unable to update parent project.");
                  } finally {
                    setParentSaving(false);
                  }
                }}
              >
                {parentSaving ? "Saving..." : "Save Parent"}
              </button>
              {parentProjectId ? (
                <button
                  className={styles.parentClear}
                  type="button"
                  disabled={parentSaving}
                  onClick={async () => {
                    if (!onUpdateParentProject) {
                      return;
                    }
                    setParentSaving(true);
                    setParentError(null);
                    try {
                      await onUpdateParentProject(null);
                      setParentQuery("");
                      setParentSelection(null);
                      setCurrentParentName(null);
                      if (onRefresh) {
                        onRefresh();
                      }
                    } catch {
                      setParentError("Unable to clear parent project.");
                    } finally {
                      setParentSaving(false);
                    }
                  }}
                >
                  Clear Parent
                </button>
              ) : null}
            </div>
            {parentError ? <div className={styles.parentError}>{parentError}</div> : null}
          </div>
        ) : null}
      </div>
    </div>
  );

  const maturityOptions = ["Sandbox", "Incubating", "Graduated", "Archived"];
  const allowedTransitions = maturityOptions.filter((option) => option !== maturity);

  const legacyContent = (
    <div className={styles.legacyStack}>
      <div className={styles.legacyIntro}>
        Compare the Maintainer DB roster with the Legacy Maintainer File. Use ADD MAINTAINER to add any maintainers listed
        in the project file who are missing from the DB.
      </div>
      <div className={styles.legacyGrid}>
        <div className={styles.column}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle}>PRESENT IN CNCF DATABASE</h2>
          </div>

          <div className={styles.section}>
            {maintainers.length === 0 ? (
              <div className={styles.empty}>No maintainers found.</div>
            ) : (
              renderMaintainerGroups()
            )}
          </div>
        </div>

        <div className={styles.column}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle}>Legacy Maintainer File</h2>
            {canEdit && onUpdateMaintainerRef ? (
              <button
                className={styles.refEditButton}
                type="button"
                onClick={() => {
                  setRefEditing((value) => !value);
                  setRefError(null);
                  if (refUrl) {
                    setRefInput(refUrl);
                  }
                }}
              >
                {refEditing ? "Cancel" : "Edit Link"}
              </button>
            ) : null}
            {refUrl ? (
              <a className={styles.refLink} href={refUrl} target="_blank" rel="noreferrer">
                {refUrl}
              </a>
            ) : null}
          </div>

          <div className={`${styles.section} ${styles.ownersSection}`}>
            {canEdit && onUpdateMaintainerRef && (refEditing || !refUrl || isRefBroken) ? (
              <div className={styles.refMissing}>
                <div className={styles.refMissingText}>
                  {!refUrl
                    ? "No project admin file is registered for this project."
                    : isRefBroken
                    ? "The project admin file could not be loaded. Update the URL below."
                    : "Update Legacy Maintainer File."}
                </div>
                <div className={styles.refInputRow}>
                  <input
                    className={styles.refInput}
                    type="url"
                    placeholder="https://github.com/org/repo/blob/main/MAINTAINERS.md"
                    value={refInput}
                    onChange={(event) => {
                      setRefInput(event.target.value);
                      setRefError(null);
                    }}
                  />
                  <button
                    className={styles.refSaveButton}
                    type="button"
                    disabled={refSaving || refInput.trim() === ""}
                    onClick={async () => {
                      if (!onUpdateMaintainerRef) {
                        return;
                      }
                      const next = refInput.trim();
                      if (!next) {
                        setRefError("Enter a URL for the project admin file.");
                        return;
                      }
                      setRefSaving(true);
                      setRefError(null);
                      try {
                        await onUpdateMaintainerRef(next);
                        setRefEditing(false);
                        setRefInput("");
                        if (onRefresh) {
                          onRefresh();
                        }
                      } catch {
                        setRefError("Unable to update project admin file.");
                      } finally {
                        setRefSaving(false);
                      }
                    }}
                  >
                    {refSaving ? "Saving..." : "Save"}
                  </button>
                </div>
                {refError ? <div className={styles.refError}>{refError}</div> : null}
              </div>
            ) : null}
            {refBody ? (
              isYamlRef(refUrl) ? (
                <div className={styles.refYaml}>
                  <SyntaxHighlighter
                    language="yaml"
                    style={atomDark}
                    customStyle={{ margin: 0, background: "transparent" }}
                    codeTagProps={{ style: { fontFamily: "var(--font-geist-mono)" } }}
                  >
                    {refBody}
                  </SyntaxHighlighter>
                </div>
              ) : (
                <div className={styles.refMarkdown}>
                  <ReactMarkdown
                    remarkPlugins={[remarkGfm]}
                    rehypePlugins={[rehypeRaw, [rehypeSanitize, maintainerRefSchema]]}
                  >
                    {refBody}
                  </ReactMarkdown>
                </div>
              )
            ) : (
              <div className={styles.empty}>
                {refStatus === "fetched" ? "No maintainer ref contents available." : "Maintainer ref not available."}
              </div>
            )}
          </div>
        </div>

        <div className={styles.column}>
          <div className={styles.sectionHeader}>
            <h2 className={styles.sectionTitle}>NOT PRESENT ON CNCF DATABASE</h2>
          </div>
          <div className={styles.section}>
            {refOnlyGitHub.length === 0 ? (
              <div className={styles.empty}>None detected.</div>
            ) : (
              <ul className={styles.list}>
                {refOnlyGitHub.map((handle) => (
                  <li key={handle} className={styles.listItem}>
                    <div className={styles.listRow}>
                    {canEdit ? (
                      <button
                        className={styles.addButton}
                        type="button"
                          onClick={() => {
                            setDraft({
                              githubHandle: handle,
                              name: "",
                              email: "",
                              company: "",
                              companyMode: "select",
                              refLine: normalizedRefLines[handle.toLowerCase()] || "",
                            });
                            setModalOpen(true);
                        }}
                      >
                        ADD MAINTAINER
                      </button>
                    ) : null}
                    <a className={styles.link} href={`https://github.com/${handle}`} target="_blank" rel="noreferrer">
                      @{handle}
                    </a>
                  </div>
                </li>
              ))}
              </ul>
            )}
          </div>
        </div>
      </div>
    </div>
  );

  const menuItems = [
    { id: "legacy", label: "MAINTAINER ROLL CALL" },
    { id: "dot-project", label: "PROJECT STRUCTURE" },
    { id: "license-checker", label: "SERVICES / LICENSE CHECKER" },
    { id: "mailing-maintainers", label: "SERVICES / MAILING LISTS / MAINTAINERS" },
    { id: "mailing-security", label: "SERVICES / MAILING LISTS / SECURITY" },
    { id: "docs", label: "SERVICES / DOCUMENTATION" },
    { id: "slack", label: "SERVICES / COLLABORATION / SLACK" },
    { id: "discord", label: "SERVICES / COLLABORATION / DISCORD" },
  ];

  const renderContent = () => {
    switch (activeSection) {
      case "legacy":
        return (
          <>
            {legacyContent}
            <ProjectDiffControl
              status={refStatus}
              checkedAt={refCheckedAt}
              matchCount={refMatchCount}
              missingCount={refMissingCount}
              refOnlyCount={refOnlyCount}
              onRefresh={onRefresh}
              isRefreshing={isRefreshing}
            />
          </>
        );
      case "dot-project":
        return dotProjectSection;
      case "license-checker":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>License Checker</h3>
            <p className={styles.stub}>Placeholder for FOSSA / Snyk license compliance data.</p>
          </div>
        );
      case "mailing-maintainers":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>Mailing Lists · Maintainers</h3>
            <p className={styles.stub}>Placeholder for maintainer mailing list references (Groups.io / Google Groups).</p>
          </div>
        );
      case "mailing-security":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>Mailing Lists · Security</h3>
            <p className={styles.stub}>Placeholder for security mailing list references.</p>
          </div>
        );
      case "docs":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>Documentation</h3>
            <p className={styles.stub}>Placeholder for documentation hosting details.</p>
          </div>
        );
      case "slack":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>Collaboration · Slack</h3>
            <p className={styles.stub}>Placeholder for Slack workspace/channel references.</p>
          </div>
        );
      case "discord":
        return (
          <div className={styles.section}>
            <h3 className={styles.subSectionTitle}>Collaboration · Discord</h3>
            <p className={styles.stub}>Placeholder for Discord server/channel references.</p>
          </div>
        );
      default:
        return null;
    }
  };

  return (
    <Card hoverable={false} className={styles.card}>
      <div className={styles.content}>
        <div className={styles.topRow}>
          <div className={styles.header}>
            <div>
              <h1 className={styles.name}>{name || "Unknown project"}</h1>
              <p className={styles.subTitle}>{maturity || "—"}</p>
            </div>
            <div className={styles.meta}>
              <span className={styles.metaItem}>Imported from google worksheet on {formatDate(createdAt)}</span>
              <span className={styles.metaItem}>Last edited {formatDate(updatedAt)}</span>
              {updatedBy ? (
                <span className={styles.metaItem}>
                  Updated by{" "}
                  {updatedAuditId ? (
                    <Link className={styles.metaLink} href={`/audit?entry=${updatedAuditId}`}>
                      {updatedBy}
                    </Link>
                  ) : (
                    updatedBy
                  )}
                </span>
              ) : null}
            </div>
            {canEdit && onUpdateMaturity && allowedTransitions.length > 0 ? (
              <button
                className={styles.transitionButton}
                type="button"
                onClick={() => {
                  setMaturityModalOpen(true);
                  setMaturityError(null);
                }}
              >
                Transition
              </button>
            ) : null}
          </div>

        </div>

        <div className={styles.bottomRow}>
          <div className={styles.menuColumn}>
            <div className={styles.projectMenu}>
              {menuItems.map((item) => (
                <button
                  key={item.id}
                  type="button"
                  className={`${styles.menuItem} ${activeSection === item.id ? styles.menuItemActive : ""}`}
                  onClick={() => setActiveSection(item.id)}
                >
                  {item.label}
                </button>
              ))}
            </div>
          </div>
          <div className={styles.contentColumn}>
            <div className={styles.nestedCard}>
              <div className={styles.collapsibleHeader}>
                <h2 className={styles.sectionTitle}>{menuItems.find((m) => m.id === activeSection)?.label}</h2>
              </div>
              {renderContent()}
            </div>
          </div>
        </div>

        {modalOpen && draft ? (
          <ProjectAddMaintainerModal
            draft={draft}
            onClose={() => setModalOpen(false)}
            onChange={(next) => setDraft(next)}
            companyOptions={companyOptions}
            onSubmit={async () => {
              if (!onAddMaintainer || !draft) {
                return;
              }
              await onAddMaintainer(draft);
              setModalOpen(false);
            }}
          />
        ) : null}
        {maturityModalOpen ? (
          <div className={styles.modalOverlay} role="dialog" aria-modal="true">
            <div className={styles.modal}>
              <div className={styles.modalHeader}>
                <h2 className={styles.modalTitle}>Transition Project Status</h2>
                <button
                  className={styles.modalClose}
                  type="button"
                  onClick={() => setMaturityModalOpen(false)}
                >
                  Close
                </button>
              </div>
              <div className={styles.modalBody}>
                <div className={styles.modalRow}>
                  <span className={styles.modalLabel}>Current</span>
                  <span className={styles.modalValue}>{maturity || "—"}</span>
                </div>
                <div className={styles.modalRow}>
                  <span className={styles.modalLabel}>Next state</span>
                  <div className={styles.transitionOptions}>
                    {allowedTransitions.map((next) => (
                      <button
                        key={next}
                        className={styles.transitionOption}
                        type="button"
                        disabled={maturitySaving}
                        onClick={async () => {
                          if (!onUpdateMaturity) {
                            return;
                          }
                          setMaturitySaving(true);
                          setMaturityError(null);
                          try {
                            await onUpdateMaturity(next);
                            setMaturityModalOpen(false);
                          } catch {
                            setMaturityError("Unable to update project status.");
                          } finally {
                            setMaturitySaving(false);
                          }
                        }}
                      >
                        {next}
                      </button>
                    ))}
                  </div>
                </div>
                {maturityError ? <div className={styles.modalError}>{maturityError}</div> : null}
              </div>
            </div>
          </div>
        ) : null}
      </div>
    </Card>
  );
}
