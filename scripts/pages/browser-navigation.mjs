const navigationTimeout = 30000;

export async function navigate(page, url) {
  let reportFailure;
  const failure = new Promise(resolve => { reportFailure = resolve; });
  const observe = response => {
    if (response.status() >= 500 && response.request().isNavigationRequest()
      && response.frame() === page.mainFrame()) reportFailure(response);
  };
  page.on('response', observe);
  try {
    return await Promise.race([page.goto(url, { waitUntil: 'load', timeout: navigationTimeout }), failure]);
  } finally {
    page.off('response', observe);
  }
}
