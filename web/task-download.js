'use strict';

// Let the browser stream to its download manager without a multi-GiB Blob.
function downloadTaskArchive(id, token) {
  const form = document.createElement('form');
  form.method = 'POST';
  form.action = '/api/tasks/archive?id=' + encodeURIComponent(id);
  form.target = '_blank';
  form.rel = 'noopener';
  form.hidden = true;
  const input = document.createElement('input');
  input.type = 'hidden'; input.name = 'token'; input.value = token;
  form.appendChild(input);
  document.body.appendChild(form);
  try { form.submit(); } finally { form.remove(); }
}

if (typeof module !== 'undefined') module.exports = {downloadTaskArchive};
