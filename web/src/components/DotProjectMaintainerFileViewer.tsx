"use client";

import { useMemo, useState } from "react";
import { isMap, isScalar, isSeq, parseDocument, type Node } from "yaml";
import GitHubHandle from "./GitHubHandle";
import styles from "./ProjectReconciliationCard.module.css";

type KnownMaintainer = {
  id: number;
  name: string;
  github: string;
  status?: string;
};

type DotProjectMaintainerFileViewerProps = {
  source: string;
  filename: string;
  maintainers: KnownMaintainer[];
  projectId: number;
  apiBaseUrl: string;
  canEdit?: boolean;
  onAddMissingMaintainer?: (handle: string, refLine: string) => void;
};

type MaintainerTeam = {
  name: string;
  members: string[];
};

type SourceToken = {
  text: string;
  tone?: "comment" | "key" | "punctuation" | "scalar";
  maintainerHandle?: string;
};

type PatchPreviewLine = {
  key: string;
  text: string;
  lineNumber: number | null;
  tone: "context" | "added" | "deleted" | "empty";
};

type PatchPreview = {
  existing: PatchPreviewLine[];
  proposed: PatchPreviewLine[];
  addedCount: number;
  deletedCount: number;
  error?: string;
};

type PullRequestResult = {
  url: string;
  number: number;
  branch: string;
  baseBranch: string;
  filePath: string;
  addedHandles: string[];
  removedPlaceholders: string[];
};

const scalarText = (node: unknown): string => {
  if (!isScalar(node)) {
    return "";
  }
  return String(node.value ?? "").trim();
};

const mapValue = (node: unknown, key: string): unknown => {
  if (!isMap(node)) {
    return undefined;
  }
  const pair = node.items.find((item) => scalarText(item.key) === key);
  return pair?.value;
};

const scalarSeqValues = (node: unknown): string[] => {
  if (!isSeq(node)) {
    return [];
  }
  return node.items.map(scalarText).filter(Boolean);
};

const normalizeHandle = (handle: string): string => handle.trim().replace(/^@/, "").toLowerCase();

const parseMaintainerTeams = (source: string): { teams: MaintainerTeam[]; errors: string[] } => {
  const document = parseDocument<Node>(source, { keepSourceTokens: true });
  const maintainers = mapValue(document.contents, "maintainers");
  const teams: MaintainerTeam[] = [];

  if (isSeq(maintainers)) {
    for (const maintainerGroup of maintainers.items) {
      const groupTeams = mapValue(maintainerGroup, "teams");
      if (!isSeq(groupTeams)) {
        continue;
      }
      for (const team of groupTeams.items) {
        const name = scalarText(mapValue(team, "name"));
        if (!name) {
          continue;
        }
        teams.push({
          name,
          members: scalarSeqValues(mapValue(team, "members")),
        });
      }
    }
  }

  return {
    teams,
    errors: document.errors.map((error) => error.message),
  };
};

const findCommentStart = (line: string): number => {
  let quote: "'" | "\"" | null = null;

  for (let index = 0; index < line.length; index += 1) {
    const char = line[index];
    const previous = index > 0 ? line[index - 1] : "";

    if (char === "\"" && quote !== "'" && previous !== "\\") {
      quote = quote === "\"" ? null : "\"";
      continue;
    }
    if (char === "'" && quote !== "\"") {
      quote = quote === "'" ? null : "'";
      continue;
    }
    if (char === "#" && !quote && (index === 0 || /\s/.test(previous))) {
      return index;
    }
  }

  return -1;
};

const tokenHandle = (value: string, projectMaintainerHandles: Set<string>): string | undefined => {
  const normalized = normalizeHandle(value);
  return projectMaintainerHandles.has(normalized) ? normalized : undefined;
};

const tokenizeYamlCode = (code: string, projectMaintainerHandles: Set<string>): SourceToken[] => {
  if (!code) {
    return [];
  }

  const indentMatch = code.match(/^(\s*)(-\s*)?/);
  const indent = indentMatch?.[1] ?? "";
  const dash = indentMatch?.[2] ?? "";
  const contentStart = indent.length + dash.length;
  const content = code.slice(contentStart);
  const tokens: SourceToken[] = [];

  if (indent) {
    tokens.push({ text: indent });
  }
  if (dash) {
    tokens.push({ text: dash, tone: "punctuation" });
  }

  const keyMatch = content.match(/^([^:\s][^:]*)(:)(\s*)(.*)$/);
  if (keyMatch) {
    tokens.push({ text: keyMatch[1], tone: "key" });
    tokens.push({ text: keyMatch[2], tone: "punctuation" });
    if (keyMatch[3]) {
      tokens.push({ text: keyMatch[3] });
    }
    if (keyMatch[4]) {
      tokens.push({
        text: keyMatch[4],
        tone: "scalar",
        maintainerHandle: tokenHandle(keyMatch[4], projectMaintainerHandles),
      });
    }
    return tokens;
  }

  if (content) {
    tokens.push({
      text: content,
      tone: "scalar",
      maintainerHandle: tokenHandle(content, projectMaintainerHandles),
    });
  }

  return tokens;
};

const tokenizeYamlLine = (line: string, projectMaintainerHandles: Set<string>): SourceToken[] => {
  const commentStart = findCommentStart(line);
  if (commentStart < 0) {
    return tokenizeYamlCode(line, projectMaintainerHandles);
  }

  return [
    ...tokenizeYamlCode(line.slice(0, commentStart), projectMaintainerHandles),
    { text: line.slice(commentStart), tone: "comment" },
  ];
};

const tokenClassName = (token: SourceToken): string | undefined => {
  switch (token.tone) {
    case "comment":
      return styles.yamlTokenComment;
    case "key":
      return styles.yamlTokenKey;
    case "punctuation":
      return styles.yamlTokenPunctuation;
    case "scalar":
      return styles.yamlTokenScalar;
    default:
      return undefined;
  }
};

const activeMaintainerHandles = (maintainers: KnownMaintainer[]): KnownMaintainer[] => {
  const seen = new Set<string>();
  const active: KnownMaintainer[] = [];

  for (const maintainer of maintainers) {
    if ((maintainer.status || "").toLowerCase() !== "active") {
      continue;
    }
    const normalized = normalizeHandle(maintainer.github);
    if (!normalized || seen.has(normalized)) {
      continue;
    }
    seen.add(normalized);
    active.push(maintainer);
  }

  return active;
};

const leadingWhitespaceLength = (line: string): number => line.match(/^\s*/)?.[0].length ?? 0;

// Matches a `- name: <anything>` list item under a `teams:` sequence. It
// used to require the literal name "project-maintainers" - upstream
// (cncf/automation#602) dropped that requirement, so every team is now a
// maintainer team regardless of its name. The patch-preview write path
// still has to pick one team to patch into, so it targets whichever team
// comes first in the file.
const isMaintainerTeamNameLine = (line: string): boolean => /^\s*(?:-\s*)?name:\s*\S.*$/i.test(line);

const isMembersLine = (line: string): boolean => /^\s*members:\s*(?:#.*)?$/i.test(line);

const isSequenceItemLine = (line: string): boolean => /^\s*-\s*\S/.test(line);

const isMaintainerPlaceholderLine = (line: string): boolean => {
  const trimmed = line.trim();
  return /^#\s*TODO\b.*$/i.test(trimmed) || /^-\s*github-handle\s*(?:#.*)?$/i.test(trimmed);
};

const splitSourceLines = (source: string): string[] =>
  source.replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n");

const buildPatchRows = (
  originalLines: string[],
  proposedLines: string[],
  insertedAt: number,
  insertedCount: number,
): PatchPreview => {
  const existing: PatchPreviewLine[] = [];
  const proposed: PatchPreviewLine[] = [];

  for (let index = 0; index <= originalLines.length; index += 1) {
    if (index === insertedAt) {
      for (let offset = 0; offset < insertedCount; offset += 1) {
        const proposedIndex = insertedAt + offset;
        existing.push({
          key: `existing-added-${offset}`,
          text: "",
          lineNumber: null,
          tone: "empty",
        });
        proposed.push({
          key: `proposed-added-${offset}`,
          text: proposedLines[proposedIndex] ?? "",
          lineNumber: proposedIndex + 1,
          tone: "added",
        });
      }
    }

    if (index >= originalLines.length) {
      continue;
    }
    const proposedIndex = index + (index >= insertedAt ? insertedCount : 0);
    existing.push({
      key: `existing-${index}`,
      text: originalLines[index],
      lineNumber: index + 1,
      tone: "context",
    });
    proposed.push({
      key: `proposed-${proposedIndex}`,
      text: proposedLines[proposedIndex] ?? "",
      lineNumber: proposedIndex + 1,
      tone: "context",
    });
  }

  return {
    existing,
    proposed,
    addedCount: insertedCount,
    deletedCount: 0,
  };
};

const buildPatchRowsWithDeleted = (
  originalLines: string[],
  proposedLines: string[],
  insertedAt: number,
  insertedCount: number,
  deletedIndexes: number[],
): PatchPreview => {
  const existing: PatchPreviewLine[] = [];
  const proposed: PatchPreviewLine[] = [];
  const deleted = new Set(deletedIndexes);
  let proposedIndex = 0;

  for (let index = 0; index <= originalLines.length; index += 1) {
    if (index === insertedAt) {
      for (let offset = 0; offset < insertedCount; offset += 1) {
        existing.push({
          key: `existing-added-${offset}`,
          text: "",
          lineNumber: null,
          tone: "empty",
        });
        proposed.push({
          key: `proposed-added-${offset}`,
          text: proposedLines[proposedIndex] ?? "",
          lineNumber: proposedIndex + 1,
          tone: "added",
        });
        proposedIndex += 1;
      }
    }

    if (index >= originalLines.length) {
      continue;
    }
    if (deleted.has(index)) {
      existing.push({
        key: `existing-deleted-${index}`,
        text: originalLines[index],
        lineNumber: index + 1,
        tone: "deleted",
      });
      proposed.push({
        key: `proposed-deleted-${index}`,
        text: "",
        lineNumber: null,
        tone: "empty",
      });
      continue;
    }

    existing.push({
      key: `existing-${index}`,
      text: originalLines[index],
      lineNumber: index + 1,
      tone: "context",
    });
    proposed.push({
      key: `proposed-${proposedIndex}`,
      text: proposedLines[proposedIndex] ?? "",
      lineNumber: proposedIndex + 1,
      tone: "context",
    });
    proposedIndex += 1;
  }

  return {
    existing,
    proposed,
    addedCount: insertedCount,
    deletedCount: deletedIndexes.length,
  };
};

const buildMaintainerPatchPreview = (source: string, missingHandles: string[]): PatchPreview => {
  const uniqueMissingHandles = Array.from(
    new Map(missingHandles.map((handle) => [normalizeHandle(handle), handle.trim()])).values(),
  ).filter(Boolean);

  if (uniqueMissingHandles.length === 0) {
    return {
      existing: [],
      proposed: [],
      addedCount: 0,
      deletedCount: 0,
    };
  }

  const originalLines = splitSourceLines(source);
  const teamNameIndex = originalLines.findIndex(isMaintainerTeamNameLine);

  if (teamNameIndex < 0) {
    return {
      existing: [],
      proposed: [],
      addedCount: 0,
      deletedCount: 0,
      error: "Cannot generate a patch preview because no maintainer team was found.",
    };
  }

  const teamIndent = leadingWhitespaceLength(originalLines[teamNameIndex]);
  let membersIndex = -1;

  for (let index = teamNameIndex + 1; index < originalLines.length; index += 1) {
    const line = originalLines[index];
    const trimmed = line.trim();
    if (trimmed && leadingWhitespaceLength(line) <= teamIndent && isSequenceItemLine(line)) {
      break;
    }
    if (isMembersLine(line)) {
      membersIndex = index;
      break;
    }
  }

  const proposedLines = [...originalLines];

  if (membersIndex < 0) {
    const insertLines = [`${" ".repeat(teamIndent + 2)}members:`, ...uniqueMissingHandles.map((handle) => `${" ".repeat(teamIndent + 4)}- ${handle}`)];
    const insertedAt = teamNameIndex + 1;
    proposedLines.splice(insertedAt, 0, ...insertLines);
    return buildPatchRows(originalLines, proposedLines, insertedAt, insertLines.length);
  }

  const membersIndent = leadingWhitespaceLength(originalLines[membersIndex]);
  let insertAt = membersIndex + 1;
  let itemIndent = " ".repeat(membersIndent + 2);
  const placeholderIndexes: number[] = [];

  for (let index = membersIndex + 1; index < originalLines.length; index += 1) {
    const line = originalLines[index];
    const trimmed = line.trim();
    if (!trimmed) {
      continue;
    }
    const lineIndent = leadingWhitespaceLength(line);
    if (lineIndent <= membersIndent) {
      break;
    }
    if (isMaintainerPlaceholderLine(line)) {
      placeholderIndexes.push(index);
      continue;
    }
    if (isSequenceItemLine(line)) {
      itemIndent = line.match(/^\s*/)?.[0] ?? itemIndent;
      insertAt = index + 1;
      continue;
    }
    insertAt = index + 1;
  }

  const originalInsertAt = insertAt;
  for (const index of [...placeholderIndexes].reverse()) {
    if (index < insertAt) {
      insertAt -= 1;
    }
    proposedLines.splice(index, 1);
  }

  const insertLines = uniqueMissingHandles.map((handle) => `${itemIndent}- ${handle}`);
  proposedLines.splice(insertAt, 0, ...insertLines);
  return buildPatchRowsWithDeleted(originalLines, proposedLines, originalInsertAt, insertLines.length, placeholderIndexes);
};

const diffLineClassName = (tone: PatchPreviewLine["tone"]): string => {
  switch (tone) {
    case "added":
      return `${styles.dotProjectSourceLine} ${styles.dotProjectDiffLineAdded}`;
    case "deleted":
      return `${styles.dotProjectSourceLine} ${styles.dotProjectDiffLineDeleted}`;
    case "empty":
      return `${styles.dotProjectSourceLine} ${styles.dotProjectDiffLineEmpty}`;
    default:
      return styles.dotProjectSourceLine;
  }
};

const diffLineNumberClassName = (tone: PatchPreviewLine["tone"]): string => {
  switch (tone) {
    case "added":
      return `${styles.dotProjectLineNumber} ${styles.dotProjectLineNumberAdded}`;
    case "deleted":
      return `${styles.dotProjectLineNumber} ${styles.dotProjectLineNumberDeleted}`;
    default:
      return styles.dotProjectLineNumber;
  }
};

export default function DotProjectMaintainerFileViewer({
  source,
  filename,
  maintainers,
  projectId,
  apiBaseUrl,
  canEdit = false,
  onAddMissingMaintainer,
}: DotProjectMaintainerFileViewerProps) {
  const [showPatchPreview, setShowPatchPreview] = useState(true);
  const [submittingPullRequest, setSubmittingPullRequest] = useState(false);
  const [pullRequestError, setPullRequestError] = useState<string | null>(null);
  const [pullRequestResult, setPullRequestResult] = useState<PullRequestResult | null>(null);
  const parsed = useMemo(() => parseMaintainerTeams(source), [source]);
  const lines = useMemo(() => splitSourceLines(source), [source]);
  const activeMaintainers = useMemo(() => activeMaintainerHandles(maintainers), [maintainers]);
  const activeMaintainersByHandle = useMemo(() => {
    const known = new Map<string, KnownMaintainer>();
    for (const maintainer of activeMaintainers) {
      const normalized = normalizeHandle(maintainer.github);
      if (normalized) {
        known.set(normalized, maintainer);
      }
    }
    return known;
  }, [activeMaintainers]);
  // Every team in the file is a maintainer team - upstream (cncf/automation#602)
  // dropped the requirement that this be a team literally named
  // "project-maintainers". maintainer-d does not yet read upstream's
  // `managed` flag (see maintainer-d#173/#172), so as an interim rule every
  // team's members count, matching upstream's default for files that never
  // set `managed` at all.
  const allTeamMembers = useMemo(
    () => parsed.teams.flatMap((team) => team.members),
    [parsed.teams],
  );
  const maintainerTeamHandles = useMemo(
    () => new Set(allTeamMembers.map(normalizeHandle).filter(Boolean)),
    [allTeamMembers],
  );
  const memberCount = parsed.teams.reduce((total, team) => total + team.members.length, 0);
  const missingProjectMaintainers = allTeamMembers.filter(
    (member) => !activeMaintainersByHandle.has(normalizeHandle(member)),
  );
  const missingFileMaintainers = activeMaintainers.filter(
    (maintainer) => !maintainerTeamHandles.has(normalizeHandle(maintainer.github)),
  );
  const patchPreview = useMemo(
    () => buildMaintainerPatchPreview(source, missingFileMaintainers.map((maintainer) => maintainer.github)),
    [source, missingFileMaintainers],
  );

  const submitPullRequest = async () => {
    setSubmittingPullRequest(true);
    setPullRequestError(null);
    setPullRequestResult(null);
    try {
      const response = await fetch(`${apiBaseUrl}/projects/${projectId}/dot-project/pull-request`, {
        method: "POST",
        credentials: "include",
      });
      if (!response.ok) {
        const message = await response.text().catch(() => "");
        throw new Error(message.trim() || "Unable to submit pull request.");
      }
      const payload = (await response.json()) as PullRequestResult;
      setPullRequestResult(payload);
    } catch (err) {
      setPullRequestError(err instanceof Error ? err.message : "Unable to submit pull request.");
    } finally {
      setSubmittingPullRequest(false);
    }
  };

  const renderYamlLine = (line: string, keyPrefix: string) => {
    const tokens = tokenizeYamlLine(line, maintainerTeamHandles);
    return tokens.length > 0
      ? tokens.map((token, tokenIndex) =>
          token.maintainerHandle ? (
            <span className={tokenClassName(token)} key={`${keyPrefix}-${tokenIndex}-${token.text}`}>
              {renderMaintainerHandle(token.text, line)}
            </span>
          ) : (
            <span className={tokenClassName(token)} key={`${keyPrefix}-${tokenIndex}-${token.text}`}>
              {token.text}
            </span>
          ),
        )
      : "\u00a0";
  };

  const renderMaintainerHandle = (handle: string, refLine: string) => {
    const normalized = normalizeHandle(handle);
    const maintainer = activeMaintainersByHandle.get(normalized);
    if (maintainer) {
      return (
        <a className={styles.dotProjectMaintainerLink} href={`/maintainers/${maintainer.id}`}>
          {handle}
        </a>
      );
    }
    if (canEdit && onAddMissingMaintainer) {
      return (
        <button
          className={styles.dotProjectMissingMaintainerButton}
          type="button"
          onClick={() => onAddMissingMaintainer(handle, refLine)}
        >
          {handle}
        </button>
      );
    }
    return <span className={styles.dotProjectMissingMaintainer}>{handle}</span>;
  };

  return (
    <div className={styles.dotProjectYamlViewer}>
      <div className={styles.dotProjectYamlHeader}>
        <div>
          <h4 className={styles.dotProjectYamlTitle}>Formatted maintainer file</h4>
          <p className={styles.dotProjectYamlSubtitle}>
            Parsed from <code>{filename}</code>; source text remains read-only and whitespace-preserving.
          </p>
        </div>
        <div className={styles.dotProjectYamlStats}>
          <span>{parsed.teams.length} teams</span>
          <span>{memberCount} members</span>
        </div>
      </div>

      {parsed.errors.length > 0 ? (
        <div className={styles.dotProjectYamlParseError}>
          YAML parse issue: <strong>{parsed.errors[0]}</strong>
        </div>
      ) : null}
      {parsed.errors.length === 0 && parsed.teams.length === 0 ? (
        <div className={styles.dotProjectYamlParseError}>
          Validation issue: no maintainer teams were found in the cached maintainer file.
        </div>
      ) : null}
      {missingProjectMaintainers.length > 0 ? (
        <div className={styles.dotProjectYamlValidation}>
          {missingProjectMaintainers.length} maintainer handle
          {missingProjectMaintainers.length === 1 ? " is" : "s are"} not active in maintainer-d.
        </div>
      ) : null}
      {missingFileMaintainers.length > 0 ? (
        <div className={styles.dotProjectDiffSummary}>
          <div>
            <h5 className={styles.dotProjectDiffSummaryTitle}>DB-vs-file diff</h5>
            <p className={styles.dotProjectDiffSummaryBody}>
              {missingFileMaintainers.length} active maintainer-d maintainer
              {missingFileMaintainers.length === 1 ? " is" : "s are"} missing from the maintainer teams in{" "}
              <code>{filename}</code>:
            </p>
            <div className={styles.dotProjectDiffMaintainerList}>
              {missingFileMaintainers.map((maintainer) => (
                <GitHubHandle
                  github={maintainer.github}
                  id={maintainer.id}
                  key={maintainer.github}
                  name={maintainer.name}
                  prefixName
                />
              ))}
            </div>
          </div>
          <button
            className={styles.dotProjectPreviewButton}
            type="button"
            onClick={() => setShowPatchPreview((current) => !current)}
          >
            {showPatchPreview ? "Hide PR Preview" : "Show PR Preview"}
          </button>
        </div>
      ) : parsed.teams.length > 0 ? (
        <div className={styles.dotProjectYamlSuccess}>
          All active maintainer-d maintainers are present in the maintainer teams.
        </div>
      ) : null}

      {parsed.teams.length > 0 ? (
        <div className={styles.dotProjectTeamGrid}>
          {parsed.teams.map((team) => (
            <section className={styles.dotProjectTeamCard} key={team.name}>
              <div className={styles.dotProjectTeamHeader}>
                <span className={styles.dotProjectTeamName}>{team.name}</span>
                <span className={styles.dotProjectTeamMeta}>{team.members.length} members</span>
              </div>
              <div className={styles.dotProjectMemberList}>
                {team.members.map((member) => (
                  <span className={styles.dotProjectMemberChip} key={`${team.name}-${member}`}>
                    {renderMaintainerHandle(member, `- ${member}`)}
                  </span>
                ))}
              </div>
            </section>
          ))}
        </div>
      ) : (
        <div className={styles.dotProjectYamlParseError}>No maintainer teams were parsed from the cached file.</div>
      )}

      <div className={styles.dotProjectSource} aria-label={`${filename} source`}>
        {lines.map((line, lineIndex) => {
          return (
            <div className={styles.dotProjectSourceLine} key={`${lineIndex}-${line}`}>
              <span className={styles.dotProjectLineNumber}>{lineIndex + 1}</span>
              <code className={styles.dotProjectLineCode}>{renderYamlLine(line, `source-${lineIndex}`)}</code>
            </div>
          );
        })}
      </div>

      {showPatchPreview ? (
        <section className={styles.dotProjectPatchPreview} aria-label="Pull request preview">
          <div className={styles.dotProjectPatchHeader}>
            <div>
              <h4 className={styles.dotProjectYamlTitle}>Pull request preview</h4>
              <p className={styles.dotProjectYamlSubtitle}>
                Review the maintainer-d generated patch before submitting it to GitHub.
              </p>
            </div>
            <div className={styles.dotProjectPatchActions}>
              <div className={styles.dotProjectYamlStats}>
                <span>+{patchPreview.addedCount}</span>
                <span>-{patchPreview.deletedCount}</span>
              </div>
              {canEdit && !patchPreview.error ? (
                <button
                  className={styles.dotProjectPreviewButton}
                  disabled={submittingPullRequest}
                  type="button"
                  onClick={submitPullRequest}
                >
                  {submittingPullRequest ? "Submitting..." : "Submit Pull Request"}
                </button>
              ) : null}
            </div>
          </div>
          {pullRequestError ? <div className={styles.dotProjectYamlParseError}>{pullRequestError}</div> : null}
          {pullRequestResult ? (
            <div className={styles.dotProjectYamlSuccess}>
              Pull request submitted:{" "}
              <a href={pullRequestResult.url} target="_blank" rel="noreferrer">
                #{pullRequestResult.number}
              </a>{" "}
              from <code>{pullRequestResult.branch}</code> to <code>{pullRequestResult.baseBranch}</code>.
            </div>
          ) : null}
          {patchPreview.error ? (
            <div className={styles.dotProjectYamlParseError}>{patchPreview.error}</div>
          ) : (
            <div className={styles.dotProjectDiffGrid}>
              <div className={styles.dotProjectDiffPane}>
                <div className={styles.dotProjectDiffPaneHeader}>Existing {filename}</div>
                <div className={styles.dotProjectSource}>
                  {patchPreview.existing.map((line) => (
                    <div className={diffLineClassName(line.tone)} key={line.key}>
                      <span className={diffLineNumberClassName(line.tone)}>{line.lineNumber ?? ""}</span>
                      <code className={styles.dotProjectLineCode}>{renderYamlLine(line.text, line.key)}</code>
                    </div>
                  ))}
                </div>
              </div>
              <div className={styles.dotProjectDiffPane}>
                <div className={styles.dotProjectDiffPaneHeader}>Proposed {filename}</div>
                <div className={styles.dotProjectSource}>
                  {patchPreview.proposed.map((line) => (
                    <div className={diffLineClassName(line.tone)} key={line.key}>
                      <span className={diffLineNumberClassName(line.tone)}>{line.lineNumber ?? ""}</span>
                      <code className={styles.dotProjectLineCode}>{renderYamlLine(line.text, line.key)}</code>
                    </div>
                  ))}
                </div>
              </div>
            </div>
          )}
        </section>
      ) : null}
    </div>
  );
}
