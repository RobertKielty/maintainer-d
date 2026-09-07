"use client";

import { useEffect, useMemo, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import AppShell from "@/components/AppShell";
import MaintainerCard, {
  CompanyOption,
  LfxProfileSummary,
  MaintainerEditDraft,
  SanitizeStatus,
} from "@/components/MaintainerCard";
import MaintainerServicesPanel, {
  MaintainerServiceView,
} from "@/components/MaintainerServicesPanel";
import MaintainerIdentityPanel, {
  IdentityObservation,
} from "@/components/MaintainerIdentityPanel";
import LfxProfilesPanel from "@/components/LfxProfilesPanel";
import CompanyCreateModal from "@/components/CompanyCreateModal";
import { getAuthBaseUrl, redirectToAuthLogin } from "@/utils/auth";
import styles from "./page.module.css";

type MaintainerDetail = {
  id: number;
  name: string;
  email: string;
  github: string;
  githubEmail: string;
  status: string;
  companyId?: number | null;
  company?: string;
  location?: string;
  country?: string;
  timezone?: string;
  projects: { id: number; name: string; status: string; refUrl?: string }[];
  services?: MaintainerServiceView[];
  observations?: IdentityObservation[];
  createdAt: string;
  updatedAt: string;
  updatedBy?: string;
  deletedAt?: string | null;
};

const maintainerDataHasChanged = (
  current: MaintainerDetail | null,
  next: MaintainerDetail
): boolean => {
  if (!current) {
    return true;
  }
  if (
    current.id !== next.id ||
    current.name !== next.name ||
    current.email !== next.email ||
    current.github !== next.github ||
    current.githubEmail !== next.githubEmail ||
    current.company !== next.company ||
    current.companyId !== next.companyId ||
    current.location !== next.location ||
    current.country !== next.country ||
    current.timezone !== next.timezone ||
    JSON.stringify(current.services ?? []) !== JSON.stringify(next.services ?? []) ||
    current.createdAt !== next.createdAt ||
    current.updatedAt !== next.updatedAt ||
    current.updatedBy !== next.updatedBy ||
    current.deletedAt !== next.deletedAt
  ) {
    return true;
  }
  if (current.projects.length !== next.projects.length) {
    return true;
  }
  for (let index = 0; index < current.projects.length; index += 1) {
    if (
      current.projects[index].id !== next.projects[index].id ||
      current.projects[index].name !== next.projects[index].name ||
      current.projects[index].status !== next.projects[index].status ||
      current.projects[index].refUrl !== next.projects[index].refUrl
    ) {
      return true;
    }
  }
  return false;
};

export default function MaintainerPage() {
  const [maintainer, setMaintainer] = useState<MaintainerDetail | null>(null);
  const [status, setStatus] = useState<"idle" | "loading" | "ready">("idle");
  const [error, setError] = useState<string | null>(null);
  const [role, setRole] = useState<string | null>(null);
  const [selfMaintainerId, setSelfMaintainerId] = useState<number | null>(null);
  const [selfLogin, setSelfLogin] = useState<string | null>(null);
  const [companies, setCompanies] = useState<CompanyOption[]>([]);
  const [editDraft, setEditDraft] = useState<MaintainerEditDraft | null>(null);
  const [isEditing, setIsEditing] = useState(false);
  const [saveStatus, setSaveStatus] = useState<"idle" | "saving">("idle");
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveNotice, setSaveNotice] = useState<string | null>(null);
  const [isCompanyModalOpen, setIsCompanyModalOpen] = useState(false);
  const [companyDraftName, setCompanyDraftName] = useState("");
  const [selectedCompanyId, setSelectedCompanyId] = useState<number | null>(null);
  const [companySaveStatus, setCompanySaveStatus] = useState<"idle" | "saving">("idle");
  const [companySaveError, setCompanySaveError] = useState<string | null>(null);
  const [sanitizeStatus, setSanitizeStatus] = useState<SanitizeStatus | null>(null);
  const router = useRouter();
  const params = useParams<{ id: string }>();
  const maintainerId = params?.id;
  const maintainerIdNumber = maintainerId ? Number(maintainerId) : null;
  const pollIntervalMs = 5000;

  const bffBaseUrl = useMemo(() => {
    const raw = process.env.NEXT_PUBLIC_BFF_BASE_URL || "/api";
    return raw.replace(/\/+$/, "");
  }, []);
  const apiBaseUrl = useMemo(() => {
    if (bffBaseUrl === "") {
      return "/api";
    }
    if (bffBaseUrl.endsWith("/api")) {
      return bffBaseUrl;
    }
    return `${bffBaseUrl}/api`;
  }, [bffBaseUrl]);
  const authBaseUrl = useMemo(() => getAuthBaseUrl(bffBaseUrl), [bffBaseUrl]);

  const canEdit = role === "staff";
  const canEditSelf =
    role === "maintainer" &&
    ((selfMaintainerId !== null &&
      maintainerIdNumber !== null &&
      selfMaintainerId === maintainerIdNumber) ||
      (!!selfLogin &&
        !!maintainer?.github &&
        selfLogin.toLowerCase() === maintainer.github.toLowerCase()));
  const canEditRecord = canEdit || canEditSelf;
  const disableNonStaffFields = role === "maintainer";

  useEffect(() => {
    let alive = true;
    if (!maintainerId) {
      return () => {
        alive = false;
      };
    }

    const loadMaintainer = async () => {
      if (maintainer === null) {
        setStatus("loading");
        setError(null);
      }
      try {
        const response = await fetch(
          `${apiBaseUrl}/maintainers/${maintainerId}`,
          { credentials: "include" }
        );
        if (!response.ok) {
          if (response.status === 401) {
            redirectToAuthLogin(authBaseUrl, `/maintainers/${maintainerId}`);
            return;
          }
          throw new Error(`unexpected status ${response.status}`);
        }
        const data = (await response.json()) as MaintainerDetail;
        if (alive && maintainerDataHasChanged(maintainer, data)) {
          setMaintainer(data);
        }
      } catch {
        if (alive && error !== "Unable to load maintainer") {
          setError("Unable to load maintainer");
        }
      } finally {
        if (alive) {
          setStatus("ready");
        }
      }
    };

    void loadMaintainer();
    let intervalId: number | null = null;
    if (!isEditing) {
      intervalId = window.setInterval(loadMaintainer, pollIntervalMs);
    }

    return () => {
      alive = false;
      if (intervalId) {
        window.clearInterval(intervalId);
      }
    };
  }, [apiBaseUrl, authBaseUrl, error, isEditing, maintainer, maintainerId, pollIntervalMs, router]);

  useEffect(() => {
    let alive = true;
    const loadSanitizeStatus = async () => {
      try {
        const response = await fetch(`${apiBaseUrl}/sanitize-status`, {
          credentials: "include",
        });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as SanitizeStatus;
        if (alive) {
          setSanitizeStatus(data);
        }
      } catch {
        // Ignore; the freshness note simply stays hidden.
      }
    };
    void loadSanitizeStatus();
    const intervalId = window.setInterval(loadSanitizeStatus, 60000);
    return () => {
      alive = false;
      window.clearInterval(intervalId);
    };
  }, [apiBaseUrl]);

  useEffect(() => {
    let alive = true;
    const loadRole = async () => {
      try {
        const response = await fetch(`${apiBaseUrl}/me`, { credentials: "include" });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as {
          role?: string;
          maintainerId?: number;
          login?: string;
        };
        if (alive) {
          setRole(data.role || null);
          setSelfMaintainerId(
            typeof data.maintainerId === "number" ? data.maintainerId : null
          );
          setSelfLogin(data.login || null);
        }
      } catch {
        // Ignore.
      }
    };
    void loadRole();
    return () => {
      alive = false;
    };
  }, [apiBaseUrl]);

  useEffect(() => {
    let alive = true;
    if (!canEditRecord) {
      return () => {
        alive = false;
      };
    }
    const loadCompanies = async () => {
      try {
        const response = await fetch(`${apiBaseUrl}/companies`, {
          credentials: "include",
        });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as CompanyOption[];
        if (alive) {
          setCompanies(data.sort((a, b) => a.name.localeCompare(b.name)));
        }
      } catch {
        // Ignore.
      }
    };
    void loadCompanies();
    return () => {
      alive = false;
    };
  }, [apiBaseUrl, canEditRecord]);

  useEffect(() => {
    if (!maintainer || isEditing) {
      return;
    }
    setEditDraft({
      name: maintainer.name || "",
      email: maintainer.email || "",
      github: maintainer.github || "",
      githubEmail: maintainer.githubEmail || "",
      location: maintainer.location || "",
      companyId: maintainer.companyId ?? null,
    });
  }, [isEditing, maintainer]);

  const isDirty =
    !!maintainer &&
    !!editDraft &&
    (maintainer.name !== editDraft.name ||
      maintainer.email !== editDraft.email ||
      maintainer.github !== editDraft.github ||
      (maintainer.githubEmail || "") !== editDraft.githubEmail ||
      (maintainer.location || "") !== editDraft.location ||
      (maintainer.companyId ?? null) !== editDraft.companyId);

  useEffect(() => {
    if (!saveNotice) {
      return;
    }
    const timer = window.setTimeout(() => setSaveNotice(null), 6000);
    return () => window.clearTimeout(timer);
  }, [saveNotice]);

  const handleSave = async () => {
    if (!maintainer || !editDraft) {
      return;
    }
    setSaveStatus("saving");
    setSaveError(null);
    try {
      const response = await fetch(`${apiBaseUrl}/maintainers/${maintainer.id}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({
          name: editDraft.name,
          email: editDraft.email,
          github: editDraft.github,
          githubEmail: editDraft.githubEmail,
          location: editDraft.location || null,
          companyId: editDraft.companyId,
        }),
      });
      if (!response.ok) {
        throw new Error(`unexpected status ${response.status}`);
      }
      const data = (await response.json()) as MaintainerDetail;
      setMaintainer((prev) =>
        prev ? (maintainerDataHasChanged(prev, data) ? data : prev) : data
      );
      setSaveNotice("Updated just now");
      setIsEditing(false);
    } catch {
      setSaveError("Unable to update maintainer");
    } finally {
      setSaveStatus("idle");
    }
  };

  const handleCreateCompany = async () => {
    const trimmed = companyDraftName.trim();
    if (!trimmed) {
      setCompanySaveError("Company name is required");
      return;
    }
    if (selectedCompanyId) {
      setEditDraft((prev) =>
        prev ? { ...prev, companyId: selectedCompanyId } : prev
      );
      setIsCompanyModalOpen(false);
      setCompanySaveError(null);
      return;
    }
    setCompanySaveStatus("saving");
    setCompanySaveError(null);
    try {
      const response = await fetch(`${apiBaseUrl}/companies`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ name: trimmed }),
      });
      if (response.status === 409) {
        setCompanySaveError("Company already exists");
        return;
      }
      if (!response.ok) {
        throw new Error(`unexpected status ${response.status}`);
      }
      const created = (await response.json()) as CompanyOption;
      setCompanies((prev) =>
        [...prev, created].sort((a, b) => a.name.localeCompare(b.name))
      );
      setEditDraft((prev) =>
        prev ? { ...prev, companyId: created.id } : prev
      );
      setCompanyDraftName("");
      setIsCompanyModalOpen(false);
    } catch {
      setCompanySaveError("Unable to add company");
    } finally {
      setCompanySaveStatus("idle");
    }
  };

  const lfxProfile: LfxProfileSummary | null = useMemo(() => {
    const lfxObservations = (maintainer?.observations || []).filter(
      (observation) => observation.source === "lfx"
    );
    if (lfxObservations.length === 0) {
      return null;
    }
    // The chosen row wins even without an LFID: falling back to any row
    // that happens to carry one would let a duplicate profile drive the
    // summary card's OpenProfile link. Enrichment retires profile rows the
    // latest lookup no longer returns (lfx/enricher.go retireStaleProfiles),
    // so "chosen" here is never a stale leftover from a prior, more
    // ambiguous lookup.
    const best =
      lfxObservations.find((observation) => observation.matchStatus === "chosen") ||
      lfxObservations.find((observation) => observation.matchStatus === "matched") ||
      lfxObservations.find(
        (observation) =>
          observation.matchStatus !== "duplicate" && observation.matchStatus !== "error"
      );
    if (!best) {
      return null;
    }
    return { lfid: best.lfid, matchStatus: best.matchStatus };
  }, [maintainer?.observations]);

  const companySuggestions = useMemo(() => {
    const query = companyDraftName.trim().toLowerCase();
    if (query.length < 2) {
      return [];
    }
    return companies
      .filter((company) => company.name.toLowerCase().includes(query))
      .slice(0, 8);
  }, [companies, companyDraftName]);

  const handleCompanyDraftChange = (next: string) => {
    setCompanyDraftName(next);
    const trimmed = next.trim().toLowerCase();
    if (!trimmed) {
      setSelectedCompanyId(null);
      return;
    }
    const selected = companies.find((company) => company.id === selectedCompanyId);
    if (!selected || selected.name.toLowerCase() !== trimmed) {
      setSelectedCompanyId(null);
    }
  };

  const handleSelectCompany = (company: CompanyOption) => {
    setCompanyDraftName(company.name);
    setSelectedCompanyId(company.id);
  };

  return (
    <AppShell>
      <div className={styles.page}>
        <div className={styles.container}>
          {status === "loading" && <div className={styles.banner}>Loading…</div>}
          {error && <div className={styles.banner}>{error}</div>}
          {canEditRecord && isCompanyModalOpen && (
            <CompanyCreateModal
              name={companyDraftName}
              error={companySaveError}
              isSaving={companySaveStatus === "saving"}
              suggestions={companySuggestions}
              selectedCompanyId={selectedCompanyId}
              onChange={handleCompanyDraftChange}
              onSelectCompany={handleSelectCompany}
              onClose={() => {
                setIsCompanyModalOpen(false);
                setCompanySaveError(null);
              }}
              onSubmit={handleCreateCompany}
            />
          )}
          {maintainer && (
            <MaintainerCard
              name={maintainer.name}
              email={maintainer.email}
              github={maintainer.github}
              githubEmail={maintainer.githubEmail}
              status={maintainer.status}
              company={maintainer.company}
              companyId={maintainer.companyId}
              location={maintainer.location}
              country={maintainer.country}
              timezone={maintainer.timezone}
              projects={maintainer.projects}
              createdAt={maintainer.createdAt}
              updatedAt={maintainer.updatedAt}
              updatedBy={maintainer.updatedBy}
              updatedNotice={saveNotice}
              lfxProfile={lfxProfile}
              sanitizeStatus={sanitizeStatus}
              isEditing={isEditing}
              editConfig={canEditRecord && editDraft ? {
                draft: editDraft,
                companies,
                isDirty,
                saveStatus,
                saveError,
                disableName: disableNonStaffFields,
                disableGitHub: disableNonStaffFields,
                disableGitHubEmail: disableNonStaffFields,
                disableLocation: disableNonStaffFields,
                disableCompanyAdd: companySaveStatus === "saving",
                onEdit: () => { setIsEditing(true); setSaveError(null); },
                onCancel: () => {
                  setIsEditing(false);
                  setSaveError(null);
                  setEditDraft({
                    name: maintainer.name || "",
                    email: maintainer.email || "",
                    github: maintainer.github || "",
                    githubEmail: maintainer.githubEmail || "",
                    location: maintainer.location || "",
                    companyId: maintainer.companyId ?? null,
                  });
                },
                onChange: (next) => setEditDraft(next),
                onSave: handleSave,
                onAddCompany: () => {
                  setCompanyDraftName("");
                  setSelectedCompanyId(null);
                  setCompanySaveError(null);
                  setIsCompanyModalOpen(true);
                },
              } : undefined}
            />
          )}
          {role === "staff" && maintainer?.services?.length ? (
            <MaintainerServicesPanel
              apiBaseUrl={apiBaseUrl}
              disabled={saveStatus === "saving"}
              maintainerId={maintainer.id}
              onMaintainerUpdated={(next) => {
                const data = next as MaintainerDetail;
                setMaintainer((prev) =>
                  prev ? (maintainerDataHasChanged(prev, data) ? data : prev) : data
                );
              }}
              services={maintainer.services}
            />
          ) : null}
          {role === "staff" && maintainer?.observations?.length ? (
            <LfxProfilesPanel observations={maintainer.observations} />
          ) : null}
          {role === "staff" && maintainer?.observations?.length ? (
            <MaintainerIdentityPanel observations={maintainer.observations} />
          ) : null}
        </div>
      </div>
    </AppShell>
  );
}
