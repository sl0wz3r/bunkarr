// Saving a text the server sent (the recovery kit) as a file on the user's computer. The text is
// handed to the browser's download and not kept anywhere: no storage, no state after the click.

/**
 * saveTextFile offers text for download as filename (text/plain). It reports whether the browser
 * could start the download (false without Blob URLs, where the caller shows another way).
 */
export function saveTextFile(filename: string, text: string): boolean {
  if (typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') {
    return false;
  }
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.rel = 'noopener';
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    a.remove();
    return true;
  } finally {
    // The click has queued the download; the object URL is not needed any more.
    setTimeout(() => URL.revokeObjectURL(url), 0);
  }
}
