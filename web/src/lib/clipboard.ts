// Copying to the clipboard. navigator.clipboard exists only in a secure context (https, or
// http://localhost), and Bunkarr is usually opened over plain http on the LAN, where it is
// undefined: copyText then falls back to selecting a hidden textarea and execCommand('copy').

/**
 * copyText copies text and resolves true when it was copied, false when the browser refused (the
 * caller then asks the user to select the text and copy it by hand). Call it from a click handler:
 * the fallback only works inside the user's gesture, so it runs before anything is awaited.
 */
export async function copyText(text: string): Promise<boolean> {
  const clipboard = typeof navigator === 'undefined' ? undefined : navigator.clipboard;
  if (!clipboard?.writeText) {
    return legacyCopy(text);
  }
  try {
    await clipboard.writeText(text);
    return true;
  } catch {
    // Permission denied, or the document is not focused: the fallback may still work.
    return legacyCopy(text);
  }
}

function legacyCopy(text: string): boolean {
  if (typeof document === 'undefined' || typeof document.execCommand !== 'function') {
    return false;
  }
  const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', '');
  area.setAttribute('aria-hidden', 'true');
  area.style.position = 'fixed';
  area.style.top = '0';
  area.style.left = '0';
  area.style.opacity = '0';
  document.body.appendChild(area);
  try {
    area.focus();
    area.select();
    area.setSelectionRange(0, text.length);
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    area.remove();
    previous?.focus();
  }
}
