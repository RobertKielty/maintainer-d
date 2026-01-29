"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useParams, useRouter } from "next/navigation";
import AppShell from "@/components/AppShell";
import ProjectReconciliationCard, {
  AddMaintainerPayload,
} from "@/components/ProjectReconciliationCard";
import styles from "./page.module.css";

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

type ProjectDetail = {
  id: number;
  name: string;
  maturity: string;
  parentProjectId?: number | null;
  legacyMaintainerRef?: string;
  dotProjectYamlRef?: string;
  maintainerRefStatus: {
    url?: string;
    status: string;
    checkedAt?: string | null;
  };
  legacyMaintainerRefBody?: string;
  refOnlyGitHub: string[];
  refLines?: Record<string, string>;
  onboardingIssue?: string;
  mailingList?: string;
  maintainers: MaintainerSummary[];
  services: ServiceSummary[];
  createdAt: string;
  updatedAt: string;
  deletedAt?: string | null;
  updatedBy?: string | null;
  updatedAuditId?: number | null;
};

const projectDataHasChanged = (
  current: ProjectDetail | null,
  next: ProjectDetail
): boolean => {
  if (!current) {
    return true;
  }
  if (
    current.id !== next.id ||
    current.name !== next.name ||
    current.maturity !== next.maturity ||
    current.parentProjectId !== next.parentProjectId ||
    current.legacyMaintainerRef !== next.legacyMaintainerRef ||
    current.maintainerRefStatus.status !== next.maintainerRefStatus.status ||
    current.maintainerRefStatus.url !== next.maintainerRefStatus.url ||
    current.maintainerRefStatus.checkedAt !== next.maintainerRefStatus.checkedAt ||
    current.legacyMaintainerRefBody !== next.legacyMaintainerRefBody ||
    current.refOnlyGitHub.length !== next.refOnlyGitHub.length ||
    current.onboardingIssue !== next.onboardingIssue ||
    current.mailingList !== next.mailingList ||
    current.createdAt !== next.createdAt ||
    current.updatedAt !== next.updatedAt ||
    current.deletedAt !== next.deletedAt ||
    current.updatedBy !== next.updatedBy ||
    current.updatedAuditId !== next.updatedAuditId
  ) {
    return true;
  }
  if (current.maintainers.length !== next.maintainers.length) {
    return true;
  }
  for (let index = 0; index < current.maintainers.length; index += 1) {
    const currentMaintainer = current.maintainers[index];
    const nextMaintainer = next.maintainers[index];
    if (
      currentMaintainer.id !== nextMaintainer.id ||
      currentMaintainer.name !== nextMaintainer.name ||
      currentMaintainer.github !== nextMaintainer.github ||
      currentMaintainer.inMaintainerRef !== nextMaintainer.inMaintainerRef ||
      currentMaintainer.status !== nextMaintainer.status ||
      currentMaintainer.company !== nextMaintainer.company
    ) {
      return true;
    }
  }
  for (let index = 0; index < current.refOnlyGitHub.length; index += 1) {
    if (current.refOnlyGitHub[index] !== next.refOnlyGitHub[index]) {
      return true;
    }
  }
  const currentRefLines = current.refLines || {};
  const nextRefLines = next.refLines || {};
  const currentRefLineKeys = Object.keys(currentRefLines);
  const nextRefLineKeys = Object.keys(nextRefLines);
  if (currentRefLineKeys.length !== nextRefLineKeys.length) {
    return true;
  }
  for (const key of currentRefLineKeys) {
    if (currentRefLines[key] !== nextRefLines[key]) {
      return true;
    }
  }
  if (current.services.length !== next.services.length) {
    return true;
  }
  for (let index = 0; index < current.services.length; index += 1) {
    const currentService = current.services[index];
    const nextService = next.services[index];
    if (
      currentService.id !== nextService.id ||
      currentService.name !== nextService.name ||
      currentService.description !== nextService.description
    ) {
      return true;
    }
  }
  return false;
};

export default function ProjectPage() {
  const [project, setProject] = useState<ProjectDetail | null>(null);
  const [status, setStatus] = useState<"idle" | "loading" | "ready">("idle");
  const [error, setError] = useState<string | null>(null);
  const [role, setRole] = useState<string | null>(null);
  const [companies, setCompanies] = useState<string[]>([]);
  const projectRef = useRef<ProjectDetail | null>(null);
  const router = useRouter();
  const params = useParams<{ id: string }>();
  const projectId = params?.id;

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

  useEffect(() => {
    let alive = true;
    if (!projectId) {
      return () => {
        alive = false;
      };
    }

    const loadProject = async () => {
      if (projectRef.current === null) {
        setStatus("loading");
        setError(null);
      }
      try {
        const response = await fetch(
          `${apiBaseUrl}/projects/${projectId}`,
          { credentials: "include" }
        );
        if (!response.ok) {
          if (response.status === 401) {
            router.push("/");
            return;
          }
          throw new Error(`unexpected status ${response.status}`);
        }
        const data = (await response.json()) as ProjectDetail;
        if (alive && projectDataHasChanged(projectRef.current, data)) {
          projectRef.current = data;
          setProject(data);
        }
      } catch {
        if (alive) {
          setError((prev) => (prev === "Unable to load project" ? prev : "Unable to load project"));
        }
      } finally {
        if (alive) {
          setStatus("ready");
        }
      }
    };

    void loadProject();

    return () => {
      alive = false;
    };
  }, [apiBaseUrl, projectId, router]);

  useEffect(() => {
    let alive = true;
    const loadRole = async () => {
      try {
        const response = await fetch(`${apiBaseUrl}/me`, { credentials: "include" });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as { role?: string };
        if (alive) {
          setRole(data.role || null);
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
    const loadCompanies = async () => {
      if (role !== "staff") {
        return;
      }
      try {
        const response = await fetch(`${apiBaseUrl}/companies`, {
          credentials: "include",
        });
        if (!response.ok) {
          return;
        }
        const data = (await response.json()) as { name: string }[];
        if (alive) {
          setCompanies(
            data
              .map((item) => item.name)
              .filter((name) => name && name.trim() !== "")
              .sort((a, b) => a.localeCompare(b))
          );
        }
      } catch {
        // Ignore.
      }
    };
    void loadCompanies();
    return () => {
      alive = false;
    };
  }, [apiBaseUrl, role]);

  const handleRefresh = async () => {
    if (!projectId) {
      return;
    }
    setStatus("loading");
    setError(null);
    try {
      const response = await fetch(`${apiBaseUrl}/projects/${projectId}`, {
        credentials: "include",
      });
      if (!response.ok) {
        if (response.status === 401) {
          router.push("/");
          return;
        }
        throw new Error(`unexpected status ${response.status}`);
      }
      const data = (await response.json()) as ProjectDetail;
      if (projectDataHasChanged(projectRef.current, data)) {
        projectRef.current = data;
        setProject(data);
      }
    } catch {
      setError((prev) => (prev === "Unable to load project" ? prev : "Unable to load project"));
    } finally {
      setStatus("ready");
    }
  };

  return (
    <AppShell>
      <div className={styles.page}>
        <div className={styles.container}>
          {status === "loading" && <div className={styles.banner}>Loading…</div>}
          {error && <div className={styles.banner}>{error}</div>}
          {project && (
            <ProjectReconciliationCard
              name={project.name}
              maturity={project.maturity}
              maintainerRef={project.legacyMaintainerRef}
              maintainerRefStatus={project.maintainerRefStatus}
              maintainerRefBody={project.legacyMaintainerRefBody}
              refOnlyGitHub={project.refOnlyGitHub}
              refLines={project.refLines}
              onboardingIssue={project.onboardingIssue}
              mailingList={project.mailingList}
              maintainers={project.maintainers}
              services={project.services}
              createdAt={project.createdAt}
              updatedAt={project.updatedAt}
              updatedBy={project.updatedBy}
              updatedAuditId={project.updatedAuditId}
              parentProjectId={project.parentProjectId}
              apiBaseUrl={apiBaseUrl}
              onRefresh={handleRefresh}
              isRefreshing={status === "loading"}
              canEdit={role === "staff"}
              companyOptions={companies}
              onUpdateMaturity={async (next) => {
                if (!projectId) {
                  return;
                }
                const response = await fetch(`${apiBaseUrl}/projects/${projectId}/maturity`, {
                  method: "PATCH",
                  headers: { "Content-Type": "application/json" },
                  credentials: "include",
                  body: JSON.stringify({ maturity: next }),
                });
                if (!response.ok) {
                  setError("Unable to update project status");
                  throw new Error("update failed");
                }
                await handleRefresh();
              }}
              onUpdateParentProject={async (nextParentId) => {
                if (!projectId) {
                  return;
                }
                const response = await fetch(`${apiBaseUrl}/projects/${projectId}/parent`, {
                  method: "PATCH",
                  headers: { "Content-Type": "application/json" },
                  credentials: "include",
                  body: JSON.stringify({ parentProjectId: nextParentId }),
                });
                if (!response.ok) {
                  setError("Unable to update parent project");
                  throw new Error("update failed");
                }
                await handleRefresh();
              }}
              onUpdateMaintainerRef={async (nextRef) => {
                if (!projectId) {
                  return;
                }
                const response = await fetch(`${apiBaseUrl}/projects/${projectId}`, {
                  method: "PATCH",
                  headers: { "Content-Type": "application/json" },
                  credentials: "include",
                  body: JSON.stringify({ legacyMaintainerRef: nextRef }),
                });
                if (!response.ok) {
                  setError("Unable to update project admin file");
                  throw new Error("update failed");
                }
                await handleRefresh();
              }}
              onAddMaintainer={async (payload: AddMaintainerPayload) => {
                if (!projectId) {
                  return;
                }
                if (payload.companyMode === "new" && payload.company.trim() !== "") {
                  const companyResponse = await fetch(`${apiBaseUrl}/companies`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    credentials: "include",
                    body: JSON.stringify({ name: payload.company }),
                  });
                  if (companyResponse.status === 409) {
                    setError("Company already exists. Select it from the list instead.");
                    return;
                  }
                }
                const response = await fetch(`${apiBaseUrl}/maintainers/from-ref`, {
                  method: "POST",
                  headers: { "Content-Type": "application/json" },
                  credentials: "include",
                  body: JSON.stringify({
                    projectId: Number(projectId),
                    name: payload.name,
                    githubHandle: payload.githubHandle,
                    email: payload.email,
                    company: payload.company,
                  }),
                });
                if (!response.ok) {
                  setError("Unable to add maintainer");
                  return;
                }
                await handleRefresh();
              }}
              onBulkStatusChange={async (ids, status) => {
                await fetch(`${apiBaseUrl}/maintainers/status`, {
                  method: "POST",
                  headers: { "Content-Type": "application/json" },
                  credentials: "include",
                  body: JSON.stringify({ ids, status }),
                });
                await handleRefresh();
              }}
            />
          )}
        </div>
      </div>
    </AppShell>
  );
}
