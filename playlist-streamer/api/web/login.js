document.getElementById('login-form').addEventListener('submit', async event => {
  event.preventDefault();
  const error = document.getElementById('error'); error.textContent = '';
  try {
    const response = await fetch('api/auth/login', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({password:document.getElementById('password').value}) });
    const body = await response.json(); if (!response.ok) throw new Error(body.error || response.statusText);
    location.href = './';
  } catch (err) { error.textContent = err.message; document.getElementById('password').select(); }
});
