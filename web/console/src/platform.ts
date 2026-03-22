// platform.ts — detect if running inside Tauri or standalone browser.
export const IS_TAURI = "__TAURI_INTERNALS__" in window;
