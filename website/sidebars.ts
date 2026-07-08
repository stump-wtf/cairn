import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docsSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Product',
      collapsed: false,
      items: ['share-types', 'surfaces', 'annotations'],
    },
    {
      type: 'category',
      label: 'Design',
      collapsed: false,
      items: ['architecture', 'specifications'],
    },
  ],
};

export default sidebars;
