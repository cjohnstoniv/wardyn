#!/bin/sh
# ORACLE solution for build-static-site.
#
# Runs INSIDE the sandbox as the agent, cwd = the mounted workspace ($PWD).
# Heredoc-writes a complete, valid static site that satisfies every grader check:
# an <h1> with the business name, a 3-link <nav>, a <section id="menu"> with
# >=3 .menu-item list children, a linked stylesheet, and a style.css with a body
# font-family and a .menu-item rule. POSIX sh, no network.
set -u

cat > index.html <<'HTML'
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Larkspur Coffee Roasters</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header class="site-header">
    <h1>Larkspur Coffee Roasters</h1>
    <p class="tagline">Small-batch coffee, roasted in the valley.</p>
    <nav class="site-nav">
      <a href="#home">Home</a>
      <a href="#menu">Menu</a>
      <a href="#contact">Contact</a>
    </nav>
  </header>

  <main>
    <section id="menu">
      <h2>Our Coffees</h2>
      <ul class="menu-list">
        <li class="menu-item">Ethiopia Yirgacheffe &mdash; bright, floral, citrus</li>
        <li class="menu-item">Colombia Huila &mdash; caramel, balanced, smooth</li>
        <li class="menu-item">Sumatra Mandheling &mdash; earthy, bold, low acidity</li>
        <li class="menu-item">House Espresso Blend &mdash; dark chocolate, rich crema</li>
      </ul>
    </section>

    <section id="contact">
      <h2>Visit Us</h2>
      <p>123 Larkspur Lane &middot; Open daily 7am&ndash;5pm &middot; hello@larkspur.example</p>
    </section>
  </main>

  <footer>
    <p>&copy; Larkspur Coffee Roasters</p>
  </footer>
</body>
</html>
HTML

cat > style.css <<'CSS'
/* Larkspur Coffee Roasters — minimal stylesheet. */
body {
  font-family: "Helvetica Neue", Arial, sans-serif;
  margin: 0;
  color: #2b1d12;
  background: #f7f3ee;
  line-height: 1.5;
}

.site-header {
  padding: 2rem 1.5rem;
  background: #efe6da;
}

.site-nav a {
  margin-right: 1rem;
  color: #6b4f3a;
  text-decoration: none;
}

main {
  max-width: 40rem;
  margin: 0 auto;
  padding: 1.5rem;
}

.menu-list {
  list-style: none;
  padding: 0;
}

.menu-item {
  padding: 0.5rem 0;
  border-bottom: 1px solid #e0d6c8;
}
CSS

echo "solution.sh: wrote index.html + style.css" >&2
