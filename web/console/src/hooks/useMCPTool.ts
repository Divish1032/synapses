import { useState, useCallback } from "preact/hooks";
import { callTool } from "../api";

interface UseMCPToolResult<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  call: (args?: Record<string, unknown>) => Promise<T | null>;
  reset: () => void;
}

export function useMCPTool<T = any>(toolName: string, project: string): UseMCPToolResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const call = useCallback(async (args?: Record<string, unknown>): Promise<T | null> => {
    setLoading(true);
    setError(null);
    try {
      const result = await callTool<T>(toolName, project, args);
      setData(result);
      return result;
    } catch (e: any) {
      setError(e.message ?? "Unknown error");
      return null;
    } finally {
      setLoading(false);
    }
  }, [toolName, project]);

  const reset = useCallback(() => {
    setData(null);
    setError(null);
    setLoading(false);
  }, []);

  return { data, loading, error, call, reset };
}
