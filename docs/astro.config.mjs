// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// Starlight rather than a hand-built site: a docs site needs a sidebar, search,
// and readable defaults, and none of those are worth writing by hand for a
// terminal tool. Its content is plain Markdown, which is the property that
// matters most here — see internal/docs, which reads these files and fails the
// Go suite when the site names a command or a setting the harness does not have.
export default defineConfig({
	site: 'https://sonar.dev',
	integrations: [
		starlight({
			title: 'sonar',
			description:
				'A terminal coding agent for hosted models, over the API, with an API key. No local runtime.',
			social: [
				{
					icon: 'github',
					label: 'GitHub',
					href: 'https://github.com/abdul-hamid-achik/sonar',
				},
			],
			sidebar: [
				{ label: 'What sonar is', slug: 'index' },
				{ label: 'Getting started', slug: 'start' },
				{
					label: 'Using it',
					items: [
						{ label: 'Durable goals', slug: 'goals' },
						{ label: 'Approvals and permissions', slug: 'permissions' },
						{ label: 'MCP servers and tools', slug: 'mcp' },
						{ label: 'Sessions and export', slug: 'sessions' },
						{ label: 'Voice', slug: 'voice' },
					],
				},
				{ label: 'Configuration', slug: 'configuration' },
				{ label: 'Safety', slug: 'safety' },
			],
			editLink: {
				baseUrl: 'https://github.com/abdul-hamid-achik/sonar/edit/main/docs/',
			},
		}),
	],
});
