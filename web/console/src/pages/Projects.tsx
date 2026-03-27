import { useState, useEffect, useRef } from "preact/hooks";
import { get, api, callTool } from "../api";
import { useToast } from "../context/ToastContext";

interface Project {
  path: string;
  name?: string;
  hash: string;
  socket: string;
}

interface EntityRow {
  id: string;
  name: string;
  type: string;
  file: string;
  domain: string;
}

export function Projects({ onNav }: { onNav?: (r: string) => void }) {
  const { addToast } = useToast();
  const [projects, setProjects] = useState<Project[]>([]);
  const [searchProject, setSearchProject] = useState("");
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<EntityRow[]>([]);
  const [searchLoading, setSearchLoading] = useState(false);
  const [searched, setSearched] = useState(false);
  const searchDebounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  const loadProjects = async () => {
    try {
      const res = await get<any>("/api/admin/projects");
      const p = Array.isArray(res) ? res : res.projects ?? [];
      setProjects(p);
      if (p.length > 0 && !searchProject) setSearchProject(p[0].path);
    } catch {
      setProjects([]);
    }
  };

  useEffect(() => { loadProjects(); }, []);

  // Debounced entity search
  useEffect(() => {
    if (searchDebounce.current) clearTimeout(searchDebounce.current);
    if (!searchQuery.trim() || !searchProject) {
      setSearchResults([]);
      setSearched(false);
      return;
    }
    let cancelled = false;
    searchDebounce.current = setTimeout(async () => {
      setSearchLoading(true);
      try {
        const res = await callTool<any>(
          "find_entity",
          searchProject,
          { name: searchQuery.trim() },
        );
        if (!cancelled) {
          // find_entity returns compact text or { entities: [...] }
          const entities = Array.isArray(res?.entities) ? res.entities : [];
          setSearchResults(entities);
          setSearched(true);
        }
      } catch {
        if (!cancelled) setSearchResults([]);
      } finally {
        if (!cancelled) setSearchLoading(false);
      }
    }, 300);
    return () => {
      cancelled = true;
      if (searchDebounce.current) clearTimeout(searchDebounce.current);
    };
  }, [searchQuery, searchProject]);

  const addProject = async () => {
    const path = prompt("Enter project path:");
    if (!path) return;
    try {
      await api("/api/admin/projects", {
        method: "POST",
        body: JSON.stringify({ path }),
      });
      addToast("success", "Project registered");
      loadProjects();
    } catch (e: any) {
      addToast("error", `Failed: ${e.message}`);
    }
  };

  const removeProject = async (path: string) => {
    if (!confirm(`Remove ${path}?`)) return;
    try {
      await api(`/api/admin/projects?path=${encodeURIComponent(path)}`, {
        method: "DELETE",
      });
      addToast("info", "Project removed");
      loadProjects();
    } catch (e: any) {
      addToast("error", `Failed: ${e.message}`);
    }
  };

  const reindex = async (path: string) => {
    try {
      await api("/api/admin/projects/reindex", {
        method: "POST",
        body: JSON.stringify({ path }),
      });
      addToast("info", "Reindexing started");
    } catch (e: any) {
      addToast("error", `Failed: ${e.message}`);
    }
  };

  const openProject = (path: string) => {
    if (onNav) onNav(`/projects/${encodeURIComponent(path)}`);
  };

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1 className="page-title">Projects</h1>
          <span className="page-subtitle">{projects.length} project{projects.length !== 1 ? "s" : ""} indexed</span>
        </div>
        <div style={{ display: "flex", gap: 8 }}>
          <button className="btn-primary" onClick={addProject}>+ Add Project</button>
          <button className="btn-ghost" onClick={loadProjects} title="Refresh">{"\u21BB"}</button>
        </div>
      </div>

      {/* Search */}
      <div className="search-panel" style={{ marginBottom: 20 }}>
        <div style={{ display: "flex", gap: 8, marginBottom: 8 }}>
          <select
            className="select"
            value={searchProject}
            onChange={(e) => setSearchProject((e.target as HTMLSelectElement).value)}
          >
            {projects.map((p) => (
              <option key={p.path} value={p.path}>{p.path.split("/").pop()}</option>
            ))}
          </select>
          <input
            className="input"
            type="text"
            placeholder="Search entities..."
            value={searchQuery}
            onInput={(e) => setSearchQuery((e.target as HTMLInputElement).value)}
          />
        </div>
        {searchLoading && <div className="text-dim">Searching...</div>}
        {searched && !searchLoading && searchResults.length === 0 && (
          <div className="text-dim">No results</div>
        )}
        {searchResults.length > 0 && (
          <div className="an-table">
            <div className="an-table-body">
              {searchResults.slice(0, 20).map((e) => (
                <div key={e.id} className="an-table-row">
                  <div className="an-table-cell" style={{ flex: "0 0 30%" }}>{e.name}</div>
                  <div className="an-table-cell" style={{ flex: "0 0 20%" }}>{e.type}</div>
                  <div className="an-table-cell" style={{ flex: "0 0 50%" }}>{e.file}</div>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* Project grid */}
      <div className="dash-project-grid">
        {projects.map((p) => (
          <div
            key={p.path}
            className="dash-project-card dash-project-card-clickable"
            onClick={() => openProject(p.path)}
            role="button"
            tabIndex={0}
          >
            <div className="dash-project-card-header">
              <span className="dash-project-name">{p.path.split("/").pop()}</span>
            </div>
            <div className="dash-project-meta">{p.path}</div>
            <div className="card-actions" style={{ marginTop: 8 }}>
              <button
                className="icon-btn"
                title="Reindex"
                onClick={(e) => { e.stopPropagation(); reindex(p.path); }}
              >{"\u21BB"}</button>
              <button
                className="icon-btn icon-btn-danger"
                title="Remove"
                onClick={(e) => { e.stopPropagation(); removeProject(p.path); }}
              >{"\u2715"}</button>
            </div>
          </div>
        ))}
      </div>

      {projects.length === 0 && (
        <div className="empty-state-large">
          <p>No projects indexed yet</p>
          <p style={{ fontSize: 12, color: "var(--text-dim)" }}>
            Click "Add Project" to get started, or run <code>synapses init --path /your/project</code>
          </p>
        </div>
      )}
    </div>
  );
}
