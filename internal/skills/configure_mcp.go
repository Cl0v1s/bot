package skills

// configureMCPSkillDir est le sous-répertoire de la skill fournie d'office
// qui guide l'utilisateur dans la déclaration d'un serveur MCP.
const configureMCPSkillDir = "configure-mcp"

const configureMCPSkillContent = `---
name: configure-mcp
description: Guide l'utilisateur pour déclarer un serveur MCP (Model Context Protocol) dans la configuration du bot. À utiliser quand il veut ajouter, modifier, désactiver ou dépanner un serveur MCP, ou donner au bot de nouveaux outils via MCP.
---
# Configurer un serveur MCP

Les serveurs MCP sont déclarés dans le fichier ` + "`.bot_mcp.json`" + ` à la racine du
workspace. Leurs outils apparaissent au prochain démarrage du bot sous le nom
` + "`mcp_<serveur>_<outil>`" + `.

**Tes outils fichiers (create_file/edit_file/read_file) refusent ce fichier**
(protégé : il déclare des commandes lancées sous l'identité réelle). Tu
l'écris toi-même avec **run_shell en ` + "`as_real_user: true`" + `** : l'utilisateur voit la
commande exacte et la confirme. Pas d'autre contournement (pas de run_shell
sandboxé, qui échouerait ou produirait un fichier rejeté au chargement).

**Écriture** (chemin complet du workspace dans ton prompt système) : fichier
entier, créé avec des droits 0600 et appartenant à l'utilisateur réel, par ex. :

` + "```" + `
umask 077 && cat > <workspace>/.bot_mcp.json <<'EOF'
{ ... JSON complet ... }
EOF
` + "```" + `

Un seul appel ` + "`as_real_user`" + ` suffit (pas de ` + "`as_real_user_window`" + `). Si le fichier
existe déjà, lis-le d'abord (` + "`cat <workspace>/.bot_mcp.json`" + ` en as_real_user), fusionne
(ajoute/modifie l'entrée dans ` + "`mcpServers`" + ` sans écraser les autres), puis réécris
le fichier entier. Vérifie ensuite la validité (` + "`python3 -m json.tool <fichier>`" + `,
as_real_user aussi). Ne réaffiche jamais le contenu du fichier : il peut
contenir des secrets (` + "`env`" + `/` + "`headers`" + `).

**N'écris JAMAIS la configuration dans un autre fichier** (pas de
` + "`.mcp_config.json`" + `, ni de fichier « server » avec clientId/scopes...) : le bot
ne lit que ` + "`.bot_mcp.json`" + ` et ignorerait silencieusement tout autre fichier.

**OAuth** : pour un serveur distant qui exige OAuth (Slack, Notion...), ajoute
un objet ` + "`oauth`" + ` au serveur (jamais d'en-tête Authorization en plus). Le bot fait
lui-même le flux (code + PKCE) : au prochain ` + "`./bot chat`" + `, il affiche une URL et
ouvre le navigateur ; les jetons sont stockés (et rafraîchis) dans
` + "`.bot_mcp_tokens.json`" + `. En mode mail, seuls les jetons déjà obtenus servent :
dis à l'utilisateur d'autoriser d'abord une fois en mode chat.
- ` + "`\"oauth\": {}`" + ` suffit si le serveur annonce ses métadonnées et accepte
  l'enregistrement dynamique de client.
- Sinon, client pré-enregistré chez le fournisseur : ` + "`clientId`" + `, ` + "`clientSecret`" + `,
  ` + "`scopes`" + ` (liste) et ` + "`callbackPort`" + ` (port local fixe ; l'URL de redirection
  ` + "`http://localhost:<port>/callback`" + ` doit être déclarée chez le fournisseur).
  Si la découverte échoue : ` + "`authorizationUrl`" + ` + ` + "`tokenUrl`" + `.
Ne mets jamais clientId/clientSecret/scopes dans un autre fichier que
` + "`.bot_mcp.json`" + `, sous la clé ` + "`oauth`" + ` du serveur.

## Étapes

1. **Demande ce qu'il veut brancher** (accès fichiers, GitHub, base de
   données, navigateur...) et s'il connaît le serveur MCP voulu (sa
   documentation donne presque toujours la commande de lancement ou l'URL).
2. **Identifie le transport** :
   - serveur local lancé par une commande (le plus courant, ex: ` + "`npx`" + `,
     ` + "`uvx`" + `, un binaire) → champs ` + "`command`" + ` et ` + "`args`" + ` ;
   - serveur distant HTTP (Streamable HTTP) → champ ` + "`url`" + `.
   Exactement un des deux par serveur.
3. **Vérifie les prérequis** avec run_shell si besoin, ex: ` + "`which npx`" + ` ou
   ` + "`which uvx`" + ` (le lanceur doit être installé et dans le PATH du bot).
4. **Secrets** : jetons/clés API vont dans ` + "`env`" + ` (stdio) ou ` + "`headers`" + ` (http).
   Ne les demande pas dans la conversation si l'utilisateur peut les mettre
   lui-même ; rappelle-lui de ne jamais les partager ailleurs.
5. **Écris le JSON complet** (fichier entier, pas un fragment) comme décrit
   plus haut (run_shell as_real_user), en fusionnant avec l'existant.
6. **Redémarrage** : le bot charge la configuration une seule fois, au
   démarrage. Après modification, il faut quitter et relancer ` + "`./bot chat`" + `.
   Un avertissement ` + "`MCP \"nom\": ...`" + ` s'affiche au lancement si un serveur
   échoue ; les autres continuent de fonctionner.
7. **Vérifie** après redémarrage : les outils ` + "`mcp_<serveur>_*`" + ` doivent être
   disponibles ; propose d'en tester un.

## Format

` + "```" + `json
{
  "mcpServers": {
    "fichiers": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/docs"]
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_..."}
    },
    "distant": {
      "url": "https://exemple.org/mcp",
      "headers": {"Authorization": "Bearer ..."},
      "timeout": "2m"
    },
    "slack": {
      "url": "https://mcp.slack.com/mcp",
      "oauth": {"clientId": "...", "clientSecret": "...", "scopes": ["channels:history"], "callbackPort": 3118}
    },
    "ancien": {"command": "mon-serveur", "disabled": true}
  }
}
` + "```" + `

Champs par serveur : ` + "`command`" + `, ` + "`args`" + `, ` + "`env`" + `, ` + "`cwd`" + ` (stdio) ; ` + "`url`" + `,
` + "`headers`" + `, ` + "`oauth`" + ` (http) ; ` + "`timeout`" + ` (durée max d'un appel d'outil, défaut 60s) ;
` + "`disabled`" + ` (true = déclaré mais ignoré). Le nom du serveur : lettres, chiffres,
` + "`_`" + ` et ` + "`-`" + `, 32 caractères max. Tout autre champ est refusé (le fichier
entier est alors ignoré, avec un avertissement).

## Dépannage

- **Aucun outil mcp_* après redémarrage** : le fichier est-il bien nommé
  ` + "`.bot_mcp.json`" + ` (point initial) à la racine du workspace, JSON valide, et
  appartenant à l'utilisateur (pas un lien symbolique) ? Regarde le message
  d'avertissement au lancement.
- **« lancement de ... : executable file not found »** : ` + "`command`" + ` introuvable
  dans le PATH ; utilise un chemin absolu ou installe le lanceur.
- **Serveur lent au démarrage** : la connexion est abandonnée après 30s
  (premier ` + "`npx -y`" + ` qui télécharge le paquet) ; relance une fois le paquet en cache.
- **Erreur HTTP 401/403** : vérifie le jeton dans ` + "`headers`" + `.
- **« autorisation OAuth requise »** : lancer ` + "`./bot chat`" + ` et autoriser dans le navigateur.
  Pour réautoriser de zéro, l'utilisateur supprime l'entrée du serveur dans
  ` + "`.bot_mcp_tokens.json`" + `.
`
