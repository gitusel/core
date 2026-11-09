import IconForms from '@tabler/icons-svelte-runes/icons/forms';
import IconLifebuoy from '@tabler/icons-svelte-runes/icons/lifebuoy';
import IconMail from '@tabler/icons-svelte-runes/icons/mail';
import IconSend from '@tabler/icons-svelte-runes/icons/send';
import IconStack2 from '@tabler/icons-svelte-runes/icons/stack-2';
import IconWebhook from '@tabler/icons-svelte-runes/icons/webhook';
import IconWorld from '@tabler/icons-svelte-runes/icons/world';

export const channelTypes = [
  {
    id: 'portal',
    icon: IconWorld,
    navColor: 'from-ds-nav-green-from to-ds-nav-green-to',
    formColor: 'var(--ds-icon-accent-green)',
  },
  {
    id: 'form',
    icon: IconForms,
    navColor: 'from-ds-nav-teal-from to-ds-nav-teal-to',
    formColor: 'var(--ds-icon-accent-teal)',
  },
  {
    id: 'webhook',
    icon: IconWebhook,
    navColor: 'from-ds-nav-purple-from to-ds-nav-purple-to',
    formColor: 'var(--ds-icon-accent-purple)',
  },
  {
    id: 'email',
    icon: IconMail,
    navColor: 'from-ds-nav-blue-from to-ds-nav-blue-to',
    formColor: 'var(--ds-icon-accent-blue)',
  },
  {
    id: 'smtp',
    icon: IconSend,
    navColor: 'from-ds-nav-orange-from to-ds-nav-orange-to',
    formColor: 'var(--ds-icon-accent-orange)',
  },
  // Virtual type: smtp/outbound channels whose config selects Graph send.
  // Backend only knows type smtp; this entry powers the nav filter, type
  // column and create picker, with handleChannelSubmit + the config form
  // normalizing to graph_send_provider on the smtp channel.
  {
    id: 'graph',
    icon: IconSend,
    navColor: 'from-ds-nav-orange-from to-ds-nav-orange-to',
    formColor: 'var(--ds-icon-accent-orange)',
  },
];

export const allTypesEntry = {
  id: null,
  icon: IconStack2,
  navColor: 'from-ds-nav-gray-from to-ds-nav-gray-to',
};

export function getChannelTypeIcon(type) {
  return channelTypes.find((ct) => ct.id === type)?.icon ?? IconLifebuoy;
}

// getChannelConfig normalizes a channel's config to an object: the API
// returns a JSON string, the form state an object.
export function getChannelConfig(channel) {
  if (!channel?.config) return {};
  if (typeof channel.config === 'object') return channel.config;
  try {
    return JSON.parse(channel.config);
  } catch {
    return {};
  }
}

// getChannelTransportLabel renders the effective outbound transport for the
// Channels type column: a smtp/outbound channel whose config selects
// graph_send_provider sends through Microsoft Graph, not SMTP, and the type
// column must not claim otherwise. Inbound/other types keep their type label.
export function getChannelTransportLabel(channel) {
  const config = getChannelConfig(channel);
  if (channel?.type === 'smtp' && config.graph_send_provider === 'graph') {
    return 'Graph';
  }
  return channel?.type ?? '';
}
