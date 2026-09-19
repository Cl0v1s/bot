# bot

Petit harnais LLM en Go, sans dépendance externe (stdlib uniquement).

- Parle à un LLM via une API compatible OpenAI (`/v1/chat/completions`), typiquement un serveur local (llama.cpp, vLLM, Ollama en mode OpenAI, etc.).
- Mode `chat` : session interactive en console, avec streaming de la réponse.
- Mode `mail` : lit périodiquement les mails non lus d'une boîte IMAP, génère une réponse via le LLM et l'envoie automatiquement par SMTP, avec le même sujet que l'original (sans préfixe "Re:"). Le threading ("affichage conversation" de Thunderbird et consorts) repose sur les en-têtes `Message-Id`/`In-Reply-To`/`References`, générés et chaînés correctement.
- Gère l'historique de conversation et le **compacte automatiquement** (résumé via le LLM) quand le contexte estimé atteint ~90% de la fenêtre du modèle.
- Tool calling (function calling) : le LLM peut appeler des outils locaux (shell, lecture/écriture de fichier, requête HTTP). Les appels d'outils et leurs résultats sont affichés/journalisés à part, nettement séparés du texte de réponse.

## Configuration

Copier `.env.example` en `.env` et ajuster les valeurs (le fichier `.env` est chargé automatiquement s'il existe, sans écraser les variables déjà présentes dans l'environnement).

## Utilisation

```sh
go build -o bot .

./bot chat     # mode interactif console
./bot mail     # boucle de lecture/réponse automatique aux mails
```

En mode `chat` :
- `/exit` ou `/quit` — quitter
- `/reset` — vider l'historique de la conversation
- `/stats` — afficher le nombre de messages et l'estimation de tokens utilisés

Chaque tour (réponse du modèle, y compris ses éventuels appels d'outils) s'exécute en tâche de fond : vous pouvez continuer à taper pendant qu'il tourne. Les lignes tapées pendant ce temps sont mises en file d'attente et traitées automatiquement, dans l'ordre, dès que le tour en cours se termine (commandes `/exit`, `/new`... comprises). Exception : si le modèle demande une confirmation pendant son tour (permission d'accès à un répertoire, mise en place du sandbox), c'est la ligne suivante que vous tapez qui lui répond, pas la file. Un Ctrl+C annule le tour en cours et vide la file d'attente.

## Gestion du contexte

La taille du contexte est estimée grossièrement (~4 caractères/token). Quand elle atteint la fraction `CONTEXT_COMPACT_AT` (0.9 par défaut) de `LLM_CONTEXT_TOKENS`, les messages les plus anciens sont résumés en un seul message via un appel au LLM, en conservant tels quels les `CONTEXT_KEEP_LAST` derniers messages.

## Mode mail — sécurité

- `MAIL_ALLOW_FROM` : liste blanche d'expéditeurs (adresses séparées par des virgules) autorisés à déclencher une réponse automatique. Un mail d'un expéditeur non listé est marqué comme lu mais jamais répondu. Vide = tout le monde autorisé.
- `MAIL_MAX_BODY_CHARS` : troncature du corps du mail transmis au LLM (protection contre les mails abusivement longs).

## Tool calling

Le LLM peut appeler des outils locaux si le serveur le supporte (`tools`/`tool_calls` de l'API OpenAI). Outils disponibles :

- `run_shell` — exécute une commande via `sh -c`. Disponible en mode chat, et en mode mail si `MAIL_TOOLS_ENABLED=true`. **Attention en mode mail** : le corps d'un mail entrant est un contenu non fiable (voir `MAIL_ALLOW_FROM` ci-dessous, lui-même vulnérable au spoofing de l'en-tête `From:`) — l'exposer ici est un vecteur d'injection de prompt → exécution de code arbitraire, accepté en connaissance de cause. Pas de repli silencieux : si `SHELL_SANDBOX_USER_ENABLED=true`, `run_shell` n'est proposé en mode mail que si le sandbox a déjà été configuré ailleurs (mode mail n'a pas de console pour le mettre en place lui-même, voir ci-dessous), sinon il est simplement absent des outils disponibles ce cycle.
- `read_file` / `write_file` — lecture/écriture de fichiers locaux, soumis au système de permissions par répertoire ci-dessous. `write_file` reste **mode chat uniquement**. `read_file` accepte `offset`/`length` (en octets) pour lire un gros fichier par portions plutôt que de saturer le contexte : chaque appel tronqué indique en fin de résultat l'offset auquel reprendre.
- `http_get` — requête HTTP GET (http/https uniquement). Aucune protection SSRF (pas de filtrage des adresses privées/locales) : à activer en connaissance de cause si le LLM traite des entrées non fiables.
- `request_directory_access` — seul moyen pour le modèle d'obtenir l'accès à un répertoire pour `read_file`/`write_file`/`run_shell`.

Activation : `CHAT_TOOLS_ENABLED` (défaut `true`) et `MAIL_TOOLS_ENABLED` (défaut `false` ; une fois activé, expose `read_file`/`http_get`/`request_directory_access`/`run_shell` — jamais `write_file` — en mode mail). Autres réglages : `TOOLS_SHELL_TIMEOUT` (30s), `TOOLS_HTTP_TIMEOUT` (20s), `AGENT_MAX_STEPS` (8, nombre max d'allers-retours outils par tour avant abandon).

### Permissions sur les répertoires

Par défaut, **aucun répertoire n'est accessible** à `read_file`/`write_file`, à l'exception du workspace (voir plus bas) — ce verrou est appliqué dans le code (`internal/tools.DirPermissions`), pas seulement suggéré dans le prompt système : un chemin hors des répertoires autorisés est rejeté avant toute lecture/écriture disque, quoi que le modèle tente.

La liste des répertoires autorisés dynamiquement est **partagée** entre les invocations du programme via un fichier de persistance (JSON, chemin fixe `.bot_allowed_dirs.json`, non configurable) :

- Le modèle doit appeler `request_directory_access` pour demander l'accès à un répertoire.
- **Mode chat** : la demande déclenche une vraie question interactive dans la console (`autoriser ? [o/N]`) — c'est l'utilisateur, pas le modèle, qui décide. Un accès accordé est ajouté au fichier partagé et reste valable pour le reste de la session (et couvre les sous-répertoires).
- **Mode mail** : personne n'est disponible pour répondre en direct, donc toute demande y est automatiquement refusée. Le mode mail relit ce même fichier avant chaque cycle de poll : il a ainsi accès aux mêmes répertoires que ceux accordés via le mode chat, mais **ne peut jamais lui-même en ajouter**.

### Sandbox utilisateur système pour `run_shell`

`SHELL_SANDBOX_USER_ENABLED=true` fait exécuter `run_shell` sous un compte système dédié et restreint, `llm`, plutôt que sous l'utilisateur qui a lancé le programme — défense en profondeur en plus du filtre anti-commandes destructrices, pour le cas où celui-ci serait contourné.

À chaque lancement du mode chat, le programme vérifie si l'utilisateur courant peut déjà exécuter des commandes en tant que `llm` (`sudo -n -u llm true`). Si ce n'est pas le cas, il explique précisément ce qu'il va faire et demande confirmation avant d'exécuter (via `sudo`, qui demandera le mot de passe) :

1. Créer le groupe puis l'utilisateur système `llm` (compte système, sans mot de passe, sans connexion interactive) — via `dscl` sur macOS, `useradd`/`groupadd` sur Linux.
2. Ajouter l'utilisateur courant au groupe `llm`.
3. Installer une règle sudo limitée à l'utilisateur courant, autorisant uniquement `sudo -u llm` sans mot de passe (validée avec `visudo -c` avant toute installation).

**Le mode mail ne peut jamais effectuer cette configuration lui-même** (pas de console pour demander le mot de passe `sudo`) : il se contente de vérifier si le sandbox est déjà prêt (`sudo -n -u llm true`, sans jamais déclencher de prompt). S'il ne l'est pas, `run_shell` n'est simplement pas proposé en mode mail ce cycle — configurez-le au moins une fois via le mode chat pour qu'il devienne disponible aussi en mode mail.

**Aucun `chown` n'est jamais effectué.** Quand un répertoire est accordé au modèle (`request_directory_access`) alors que le sandbox est actif, seul son **groupe** Unix est changé vers `llm` (`chgrp`, récursif, plus bit setgid sur le répertoire racine accordé) — le propriétaire ne change jamais. Si ce changement de groupe échoue, l'accès est refusé plutôt que d'accorder silencieusement un accès non isolé.

Si le sandbox est demandé (`SHELL_SANDBOX_USER_ENABLED=true`) mais que sa configuration échoue ou est refusée (mode chat), ou n'est pas encore prête (mode mail), `run_shell` n'est pas proposé du tout cette session — jamais de repli silencieux vers une exécution non isolée.

Nécessite macOS ou Linux (pas testé sur d'autres plateformes).

#### Git / SSH sous le sandbox

Le compte `llm` n'a pas de répertoire personnel : sans configuration, `git` y échoue ("dubious ownership", "Please tell me who you are") et toute commande git sur un dépôt distant en SSH bloque en attendant une confirmation de clé d'hôte impossible à donner. Au premier octroi du workspace au sandbox, le programme crée automatiquement `<workspace>/.sandbox-gitconfig` (identité recopiée depuis votre config git `--global`, `safe.directory=*`, `known_hosts` dédié en `StrictHostKeyChecking=accept-new`) et le pointe via `GIT_CONFIG_GLOBAL`.

Par défaut, `llm` n'a toujours **aucune clé SSH** : un `git push`/`pull` distant échoue avec `Permission denied (publickey)`. `SANDBOX_SSH_KEY` (chemin d'une clé privée SSH de l'utilisateur courant) donne à `llm` un accès en **lecture seule** à cette clé précise (ACL, `setfacl`/`chmod +a` — jamais de `chown`, jamais de copie de la clé), et l'utilise via `core.sshCommand -i`. **Implication de sécurité assumée** : le LLM peut alors s'authentifier en SSH avec la véritable identité de l'utilisateur (git push vers ses dépôts, etc.) — le sandbox n'isole plus les opérations SSH/git comme il isole le reste. Vide (défaut si aucune clé usuelle n'est trouvée dans `~/.ssh`) = comportement précédent, `run_shell` sous sandbox ne peut pas s'authentifier en SSH.

Ce fichier n'est jamais réécrit une fois créé (y compris s'il a été édité à la main) : pour appliquer `SANDBOX_SSH_KEY` sur un workspace déjà initialisé sans cette option, supprimez `<workspace>/.sandbox-gitconfig` pour le faire régénérer.

## Workspace et skills

Au démarrage, le bot crée (si besoin) un répertoire **workspace**, `WORKSPACE_DIR` (défaut `~/bot-workspace`), et son sous-répertoire `skills/`. Ce dernier contient toujours d'office une skill `creer-une-skill` (recréée si absente, jamais si elle existe déjà) qui explique au modèle lui-même le format ci-dessous, pour qu'il puisse déclarer de nouvelles skills sans documentation externe.

Ce workspace est **toujours accessible** en lecture/écriture à `read_file`/`write_file`, en mode chat comme en mode mail, sans passer par `request_directory_access` — contrairement aux autres répertoires (voir "Permissions sur les répertoires" ci-dessus), cet accès ne dépend pas du fichier partagé de répertoires autorisés et n'est jamais retiré par un rafraîchissement de celui-ci.

### Déclarer une skill

Une skill est un fichier Markdown avec un en-tête optionnel, placé dans `WORKSPACE_DIR/skills/` :

- soit directement `skills/ma-skill.md` ;
- soit `skills/ma-skill/SKILL.md` (utile pour regrouper la skill avec d'autres fichiers annexes dans son propre sous-répertoire).

```markdown
---
name: ma-skill
description: ce que fait la skill et quand l'utiliser
---
Instructions détaillées pour le modèle, exemples, étapes à suivre...
```

Au démarrage, seuls le nom et la description de chaque skill trouvée sont ajoutés au prompt système (avec le chemin du fichier) : le corps complet n'est lu par le modèle que s'il appelle `read_file` sur ce chemin, pour ne pas saturer le contexte avec des skills non pertinentes à la conversation en cours.

## Limites connues

- Le client IMAP est minimal (LOGIN/SELECT/SEARCH UNSEEN/FETCH/STORE) : pas d'IDLE, pas d'OAuth2, pas de gestion avancée des dossiers.
- L'estimation du nombre de tokens est approximative (pas de tokenizer réel).
- En mode mail, les réponses sont envoyées automatiquement sans validation humaine.
- Le filtre anti-commandes destructrices (`rm -rf`, formatage, etc.) est heuristique (analyse du texte de la commande) : contournable par un attaquant motivé. La vraie protection reste de ne jamais exposer `run_shell` à un contexte non supervisé, et éventuellement d'activer `SHELL_SANDBOX_USER_ENABLED`.
